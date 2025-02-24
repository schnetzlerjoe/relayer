/*
Package cmd includes relayer commands
Copyright © 2020 Jack Zampolin <jack.zampolin@gmail.com>

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/cosmos/relayer/v2/internal/relaydebug"
	"github.com/cosmos/relayer/v2/relayer"
	"github.com/cosmos/relayer/v2/relayer/chains/cosmos"
	"github.com/cosmos/relayer/v2/relayer/processor"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// startCmd represents the start command
func startCmd(a *appState) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "start path_name",
		Aliases: []string{"st"},
		Short:   "Start the listening relayer on a given path",
		Args:    withUsage(cobra.MinimumNArgs(0)),
		Example: strings.TrimSpace(fmt.Sprintf(`
$ %s start           # start all configured paths
$ %s start demo-path # start the 'demo-path' path
$ %s start demo-path --max-msgs 3
$ %s start demo-path2 --max-tx-size 10`, appName, appName, appName, appName)),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, src, dst, interquery, err := a.Config.ChainsFromPath(args[0])
			if err != nil {
				return err
			}

			if err := ensureKeysExist(chains); err != nil {
				return err
			}

			maxTxSize, maxMsgLength, err := GetStartOptions(cmd)
			if err != nil {
				return err
			}

			var prometheusMetrics *processor.PrometheusMetrics

			debugAddr, err := cmd.Flags().GetString(flagDebugAddr)
			if err != nil {
				return err
			}
			if debugAddr == "" {
				a.Log.Info("Skipping debug server due to empty debug address flag")
			} else {
				ln, err := net.Listen("tcp", debugAddr)
				if err != nil {
					a.Log.Error("Failed to listen on debug address. If you have another relayer process open, use --" + flagDebugAddr + " to pick a different address.")
					return fmt.Errorf("failed to listen on debug address %q: %w", debugAddr, err)
				}
				log := a.Log.With(zap.String("sys", "debughttp"))
				log.Info("Debug server listening", zap.String("addr", debugAddr))
				prometheusMetrics = processor.NewPrometheusMetrics()
				relaydebug.StartDebugServer(cmd.Context(), log, ln, prometheusMetrics.Registry)
				for _, chain := range chains {
					if ccp, ok := chain.ChainProvider.(*cosmos.CosmosProvider); ok {
						ccp.SetMetrics(prometheusMetrics)
					}
				}
			}

			processorType, err := cmd.Flags().GetString(flagProcessor)
			if err != nil {
				return err
			}
			initialBlockHistory, err := cmd.Flags().GetUint64(flagInitialBlockHistory)
			if err != nil {
				return err
			}

			rlyErrCh := relayer.StartRelayer(cmd.Context(), a.Log, c[src], c[dst], interquery, filter, maxTxSize, maxMsgLength, a.Config.memo(cmd), processorType, initialBlockHistory)

			// NOTE: This block of code is useful for ensuring that the clients tracking each chain do not expire
			// when there are no packets flowing across the channels. It is currently a source of errors that have been
			// hard to rectify, so we are just avoiding this code path for now
			if false {
				thresholdTime := a.Viper.GetDuration(flagThresholdTime)
				eg, egCtx := errgroup.WithContext(cmd.Context())

				eg.Go(func() error {
					for {
						var timeToExpiry time.Duration
						if err = retry.Do(func() error {
							timeToExpiry, err = UpdateClientsFromChains(egCtx, c[src], c[dst], thresholdTime)
							return err
						}, retry.Context(egCtx), retry.Attempts(5), retry.Delay(time.Millisecond*500), retry.LastErrorOnly(true), retry.OnRetry(func(n uint, err error) {
							a.Log.Info(
								"Failed to update clients from chains",
								zap.String("src_chain_id", c[src].ChainID()),
								zap.String("dst_chain_id", c[dst].ChainID()),
								zap.Uint("attempt", n+1),
								zap.Uint("max_attempts", relayer.RtyAttNum),
								zap.Error(err),
							)
						})); err != nil {
							return err
						}
						select {
						case <-time.After(timeToExpiry - thresholdTime):
							// Nothing to do.
						case <-egCtx.Done():
							return egCtx.Err()
						}
					}
				})
				if err = eg.Wait(); err != nil {
					a.Log.Warn(
						"Error updating clients",
						zap.Error(err),
					)
				}
			}

			rlyErrCh := relayer.StartRelayer(
				cmd.Context(),
				a.Log,
				chains,
				paths,
				maxTxSize, maxMsgLength,
				a.Config.memo(cmd),
				clientUpdateThresholdTime,
				processorType, initialBlockHistory,
				prometheusMetrics,
			)

			// Block until the error channel sends a message.
			// The context being canceled will cause the relayer to stop,
			// so we don't want to separately monitor the ctx.Done channel,
			// because we would risk returning before the relayer cleans up.
			if err := <-rlyErrCh; err != nil && !errors.Is(err, context.Canceled) {
				a.Log.Warn(
					"Relayer start error",
					zap.Error(err),
				)
				return err
			}
			return nil
		},
	}
	cmd = updateTimeFlags(a.Viper, cmd)
	cmd = strategyFlag(a.Viper, cmd)
	cmd = debugServerFlags(a.Viper, cmd)
	cmd = processorFlag(a.Viper, cmd)
	cmd = initBlockFlag(a.Viper, cmd)
	cmd = memoFlag(a.Viper, cmd)
	return cmd
}

// GetStartOptions sets strategy specific fields.
func GetStartOptions(cmd *cobra.Command) (uint64, uint64, error) {
	maxTxSize, err := cmd.Flags().GetString(flagMaxTxSize)
	if err != nil {
		return 0, 0, err
	}

	txSize, err := strconv.ParseUint(maxTxSize, 10, 64)
	if err != nil {
		return 0, 0, err
	}

	maxMsgLength, err := cmd.Flags().GetString(flagMaxMsgLength)
	if err != nil {
		return txSize * MB, 0, err
	}

	msgLen, err := strconv.ParseUint(maxMsgLength, 10, 64)
	if err != nil {
		return txSize * MB, 0, err
	}

	return txSize * MB, msgLen, nil
}
