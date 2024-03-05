package relayer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/cosmos/ibc-go/v3/modules/core/04-channel/types"
	cosmosprocessor "github.com/cosmos/relayer/v2/relayer/chains/cosmos"
	"github.com/cosmos/relayer/v2/relayer/processor"
	"github.com/cosmos/relayer/v2/relayer/provider"
	cosmosprovider "github.com/cosmos/relayer/v2/relayer/provider/cosmos"
	"go.uber.org/zap"
)

// ActiveChannel represents an IBC channel and whether there is an active goroutine relaying packets against it.
type ActiveChannel struct {
	channel *types.IdentifiedChannel
	active  bool
}

const (
	ProcessorEvents string = "events"
	ProcessorLegacy        = "legacy"
)

// StartRelayer starts the main relaying loop and returns a channel that will contain any control-flow related errors.
func StartRelayer(
	ctx context.Context,
	log *zap.Logger,
	src, dst *Chain,
	filter ChannelFilter,
	maxTxSize, maxMsgLength uint64,
	memo string,
	processorType string,
	initialBlockHistory uint64,
) chan error {
	errorChan := make(chan error, 1)

	switch processorType {
	case ProcessorEvents:
		var filterSrc, filterDst []processor.ChannelKey

		for _, ch := range filter.ChannelList {
			ruleSrc := processor.ChannelKey{ChannelID: ch}
			ruleDst := processor.ChannelKey{CounterpartyChannelID: ch}
			filterSrc = append(filterSrc, ruleSrc)
			filterDst = append(filterDst, ruleDst)
		}
		paths := []path{{
			src: pathChain{
				provider: src.ChainProvider,
				pathEnd:  processor.NewPathEnd(src.ChainProvider.ChainId(), src.ClientID(), filter.Rule, filterSrc),
			},
			dst: pathChain{
				provider: dst.ChainProvider,
				pathEnd:  processor.NewPathEnd(dst.ChainProvider.ChainId(), dst.ClientID(), filter.Rule, filterDst),
			},
		}}

		go relayerStartEventProcessor(ctx, log, paths, initialBlockHistory, maxTxSize, maxMsgLength, memo, errorChan)
		return errorChan
	case ProcessorLegacy:
		go relayerMainLoop(ctx, log, src, dst, filter, maxTxSize, maxMsgLength, memo, errorChan)
		return errorChan
	default:
		panic(fmt.Errorf("unexpected processor type: %s, supports one of: [%s, %s]", processorType, ProcessorEvents, ProcessorLegacy))
	}
}

// TODO: intermediate types. Should combine/replace with the relayer.Chain, relayer.Path, and relayer.PathEnd structs
// as the stateless and stateful/event-based relaying mechanisms are consolidated.
type path struct {
	src pathChain
	dst pathChain
}

type pathChain struct {
	provider provider.ChainProvider
	pathEnd  processor.PathEnd
}

// chainProcessor returns the corresponding ChainProcessor implementation instance for a pathChain.
func (chain pathChain) chainProcessor(log *zap.Logger) processor.ChainProcessor {
	// Handle new ChainProcessor implementations as cases here
	switch p := chain.provider.(type) {
	case *cosmosprovider.CosmosProvider:
		return cosmosprocessor.NewCosmosChainProcessor(log, p)
	default:
		panic(fmt.Errorf("unsupported chain provider type: %T", chain.provider))
	}
}

// relayerStartEventProcessor is the main relayer process when using the event processor.
func relayerStartEventProcessor(
	ctx context.Context,
	log *zap.Logger,
	paths []path,
	initialBlockHistory uint64,
	maxTxSize,
	maxMsgLength uint64,
	memo string,
	errCh chan<- error,
) {
	defer close(errCh)

	epb := processor.NewEventProcessor()

	for _, p := range paths {
		epb = epb.
			WithChainProcessors(
				p.src.chainProcessor(log),
				p.dst.chainProcessor(log),
			).
			WithPathProcessors(processor.NewPathProcessor(
				log,
				p.src.pathEnd,
				p.dst.pathEnd,
				memo,
			))
	}

	ep := epb.
		WithInitialBlockHistory(initialBlockHistory).
		Build()

	errCh <- ep.Run(ctx)
}

// relayerMainLoop is the main loop of the relayer.
func relayerMainLoop(ctx context.Context, log *zap.Logger, src, dst *Chain, filter ChannelFilter, maxTxSize, maxMsgLength uint64, memo string, errCh chan<- error) {
	defer close(errCh)

	// Query the list of channels on the src connection.
	srcChannels, err := queryChannelsOnConnection(ctx, src)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			errCh <- err
		} else {
			errCh <- fmt.Errorf("error querying all channels on chain{%s}@connection{%s}: %w",
				src.ChainID(), src.ConnectionID(), err)
		}
		return
	}

	channels := make(chan *ActiveChannel, len(srcChannels))

	// Apply the channel filter rule (i.e. build allowlist, denylist or relay on all channels available),
	// then filter out only the channels in the OPEN state.
	srcChannels = applyChannelFilterRule(filter, srcChannels)
	srcOpenChannels := filterOpenChannels(srcChannels)

	var wg sync.WaitGroup
	for {
		// TODO once upstream changes are merged for emitting the channel version in ibc-go,
		// we will want to add back logic for finishing the channel handshake for interchain accounts.
		// Essentially the interchain accounts module will initiate the handshake and then the relayer finishes it.
		// So we will occasionally query recent txs and check the events for `ChannelOpenInit`, at which point
		// we will attempt to finish opening the channel.

		// TODO when we have completed the Chain/Path processor refactor we will be listening for
		// new channels that are being opened/created so it's possible we have no open channels to relay on
		// at startup but after some time has passed a channel needs opened and relayed on. At this point we
		// could choose to loop here until some action is needed.
		if len(srcOpenChannels) == 0 {
			errCh <- fmt.Errorf("there are no open channels to relay on")
			return
		}

		// Spin up a goroutine to relay packets & acks for each channel that isn't already being relayed against.
		for _, channel := range srcOpenChannels {
			if !channel.active {
				channel.active = true
				wg.Add(1)
				go relayUnrelayedPacketsAndAcks(ctx, log, &wg, src, dst, maxTxSize, maxMsgLength, memo, channel, channels)
			}
		}

		// Block here until one of the running goroutines exits, while accounting for the case where
		// the main context is cancelled while we are waiting for a read from the channel.
		var channel *ActiveChannel
		select {
		case channel = <-channels:
			break
		case <-ctx.Done():
			wg.Wait() // Wait here for the running goroutines to finish
			errCh <- ctx.Err()
			return
		}

		channel.active = false

		// When a goroutine exits we need to query the channel and check that it is still in OPEN state.
		var queryChannelResp *types.QueryChannelResponse
		if err = retry.Do(func() error {
			queryChannelResp, err = src.ChainProvider.QueryChannel(ctx, 0, channel.channel.ChannelId, channel.channel.PortId)
			if err != nil {
				return err
			}
			return nil
		}, retry.Context(ctx), RtyAtt, RtyDel, RtyErr, retry.OnRetry(func(n uint, err error) {
			src.log.Info(
				"Failed to query channel for updated state",
				zap.String("src_chain_id", src.ChainID()),
				zap.String("src_channel_id", channel.channel.ChannelId),
				zap.Uint("attempt", n+1),
				zap.Uint("max_attempts", RtyAttNum),
				zap.Error(err),
			)
		})); err != nil {
			errCh <- err
			return
		}

		// If the channel is no longer in OPEN state then we remove it from the map of open channels.
		if queryChannelResp.Channel.State != types.OPEN {
			delete(srcOpenChannels, channel.channel.ChannelId)
			src.log.Info(
				"Channel is no longer in open state",
				zap.String("chain_id", src.ChainID()),
				zap.String("channel_id", channel.channel.ChannelId),
				zap.String("channel_state", queryChannelResp.Channel.State.String()),
			)
		}
	}
}

// queryChannelsOnConnection queries all the channels associated with a connection on the src chain.
func queryChannelsOnConnection(ctx context.Context, src *Chain) ([]*types.IdentifiedChannel, error) {
	// Query the latest heights on src & dst
	srch, err := src.ChainProvider.QueryLatestHeight(ctx)
	if err != nil {
		return nil, err
	}

	// Query the list of channels for the connection on src
	var srcChannels []*types.IdentifiedChannel

	if err = retry.Do(func() error {
		srcChannels, err = src.ChainProvider.QueryConnectionChannels(ctx, srch, src.ConnectionID())
		return err
	}, retry.Context(ctx), RtyAtt, RtyDel, RtyErr, retry.OnRetry(func(n uint, err error) {
		src.log.Info(
			"Failed to query connection channels",
			zap.String("conn_id", src.ConnectionID()),
			zap.Uint("attempt", n+1),
			zap.Uint("max_attempts", RtyAttNum),
			zap.Error(err),
		)
	})); err != nil {
		return nil, err
	}

	return srcChannels, nil
}

// filterOpenChannels takes a slice of channels, searches for the channels in open state,
// and builds a map of ActiveChannel's from those open channels.
func filterOpenChannels(channels []*types.IdentifiedChannel) map[string]*ActiveChannel {
	openChannels := make(map[string]*ActiveChannel)

	for _, channel := range channels {
		if channel.State == types.OPEN {
			openChannels[channel.ChannelId] = &ActiveChannel{
				channel: channel,
				active:  false,
			}
		}
	}

	return openChannels
}

// applyChannelFilterRule will use the given ChannelFilter's rule and channel list to build the appropriate list of
// channels to relay on.
func applyChannelFilterRule(filter ChannelFilter, channels []*types.IdentifiedChannel) []*types.IdentifiedChannel {
	switch filter.Rule {
	case allowList:
		var filteredChans []*types.IdentifiedChannel
		for _, c := range channels {
			if filter.InChannelList(c.ChannelId) {
				filteredChans = append(filteredChans, c)
			}
		}
		return filteredChans
	case denyList:
		var filteredChans []*types.IdentifiedChannel
		for _, c := range channels {
			if filter.InChannelList(c.ChannelId) {
				continue
			}
			filteredChans = append(filteredChans, c)
		}
		return filteredChans
	default:
		// handle all channels on connection
		return channels
	}
}

// relayUnrelayedPacketsAndAcks will relay all the pending packets and acknowledgements on both the src and dst chains.
func relayUnrelayedPacketsAndAcks(ctx context.Context, log *zap.Logger, wg *sync.WaitGroup, src, dst *Chain, maxTxSize, maxMsgLength uint64, memo string, srcChannel *ActiveChannel, channels chan<- *ActiveChannel) {
	// make goroutine signal its death, whether it's a panic or a return
	defer func() {
		wg.Done()
		channels <- srcChannel
	}()

	for {
		if ok := relayUnrelayedPackets(ctx, log, src, dst, maxTxSize, maxMsgLength, memo, srcChannel.channel); !ok {
			return
		}
		if ok := relayUnrelayedAcks(ctx, log, src, dst, maxTxSize, maxMsgLength, memo, srcChannel.channel); !ok {
			return
		}

		// Wait for a second before continuing, but allow context cancellation to break the flow.
		select {
		case <-time.After(time.Second):
			// Nothing to do.
		case <-ctx.Done():
			return
		}
	}
}

// relayUnrelayedPackets fetches unrelayed packet sequence numbers and attempts to relay the associated packets.
// relayUnrelayedPackets returns true if packets were empty or were successfully relayed.
// Otherwise, it logs the errors and returns false.
func relayUnrelayedPackets(ctx context.Context, log *zap.Logger, src, dst *Chain, maxTxSize, maxMsgLength uint64, memo string, srcChannel *types.IdentifiedChannel) bool {
	st := time.Now()
	// Fetch any unrelayed sequences depending on the channel order
	sp := UnrelayedSequences(ctx, src, dst, srcChannel)
	elap_0 := time.Since(st)
	fmt.Println("sp:", sp)
	// If there are no unrelayed packets, stop early.
	if sp.Empty() {
		src.log.Debug(
			"No packets in queue",
			zap.String("src_chain_id", src.ChainID()),
			zap.String("src_channel_id", srcChannel.ChannelId),
			zap.String("src_port_id", srcChannel.PortId),
			zap.String("dst_chain_id", dst.ChainID()),
			zap.String("dst_channel_id", srcChannel.Counterparty.ChannelId),
			zap.String("dst_port_id", srcChannel.Counterparty.PortId),
		)
		return true
	}

	if len(sp.Src) > 0 {
		src.log.Info(
			"Unrelayed source packets",
			zap.String("src_chain_id", src.ChainID()),
			zap.String("src_channel_id", srcChannel.ChannelId),
			zap.Uint64s("seqs", sp.Src),
		)
	}

	if len(sp.Dst) > 0 {
		src.log.Info(
			"Unrelayed destination packets",
			zap.String("dst_chain_id", dst.ChainID()),
			zap.String("dst_channel_id", srcChannel.Counterparty.ChannelId),
			zap.Uint64s("seqs", sp.Dst),
		)
	}

	st_1 := time.Now()
	if err := RelayPackets(ctx, log, src, dst, sp, maxTxSize, maxMsgLength, memo, srcChannel); err != nil {
		// If there was a context cancellation or deadline while attempting to relay packets,
		// log that and indicate failure.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			log.Warn(
				"Context finished while waiting for RelayPackets to complete",
				zap.String("src_chain_id", src.ChainID()),
				zap.String("src_channel_id", srcChannel.ChannelId),
				zap.String("dst_chain_id", dst.ChainID()),
				zap.String("dst_channel_id", srcChannel.Counterparty.ChannelId),
				zap.Error(ctx.Err()),
			)
			return false
		}

		// If we encounter an error that suggest node configuration issues, log a more insightful error message.
		if strings.Contains(err.Error(), "Internal error: transaction indexing is disabled") {
			log.Warn(
				"Remote server needs to enable transaction indexing",
				zap.String("src_chain_id", src.ChainID()),
				zap.String("src_channel_id", srcChannel.ChannelId),
				zap.String("dst_chain_id", dst.ChainID()),
				zap.String("dst_channel_id", srcChannel.Counterparty.ChannelId),
				zap.Error(ctx.Err()),
			)
			return false
		}

		// Otherwise, not a context error, but an application-level error.
		log.Warn(
			"Relay packets error",
			zap.String("src_chain_id", src.ChainID()),
			zap.String("src_channel_id", srcChannel.ChannelId),
			zap.String("dst_chain_id", dst.ChainID()),
			zap.String("dst_channel_id", srcChannel.Counterparty.ChannelId),
			zap.Error(err),
		)
		// Indicate that we should attempt to keep going.
		return true
	}
	elap_1 := time.Since(st_1)
	elap := time.Since(st)
	fmt.Println("relayUnrelayedPackets", elap)
	fmt.Println("UnrelayedSequences", elap_0, "RelayPackets", elap_1)
	return true
}

// relayUnrelayedAcks fetches unrelayed acknowledgements and attempts to relay them.
// relayUnrelayedAcks returns true if acknowledgements were empty or were successfully relayed.
// Otherwise, it logs the errors and returns false.
func relayUnrelayedAcks(ctx context.Context, log *zap.Logger, src, dst *Chain, maxTxSize, maxMsgLength uint64, memo string, srcChannel *types.IdentifiedChannel) bool {
	// Fetch any unrelayed acks depending on the channel order
	st := time.Now()
	ap := UnrelayedAcknowledgements(ctx, src, dst, srcChannel)
	elap_0 := time.Since(st)
	fmt.Println("ap:", ap)
	// If there are no unrelayed acks, stop early.
	if ap.Empty() {
		log.Debug(
			"No acknowledgements in queue",
			zap.String("src_chain_id", src.ChainID()),
			zap.String("src_channel_id", srcChannel.ChannelId),
			zap.String("src_port_id", srcChannel.PortId),
			zap.String("dst_chain_id", dst.ChainID()),
			zap.String("dst_channel_id", srcChannel.Counterparty.ChannelId),
			zap.String("dst_port_id", srcChannel.Counterparty.PortId),
		)
		return true
	}

	if len(ap.Src) > 0 {
		log.Info(
			"Unrelayed source acknowledgements",
			zap.String("src_chain_id", src.ChainID()),
			zap.String("src_channel_id", srcChannel.ChannelId),
			zap.Uint64s("acks", ap.Src),
		)
	}

	if len(ap.Dst) > 0 {
		log.Info(
			"Unrelayed destination acknowledgements_???",
			zap.String("dst_chain_id", dst.ChainID()),
			zap.String("dst_channel_id", srcChannel.Counterparty.ChannelId),
			zap.Uint64s("acks", ap.Dst),
		)
	}
	st_1 := time.Now()
	if err := RelayAcknowledgements(ctx, log, src, dst, ap, maxTxSize, maxMsgLength, memo, srcChannel); err != nil {
		// If there was a context cancellation or deadline while attempting to relay acknowledgements,
		// log that and indicate failure.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			log.Warn(
				"Context finished while waiting for RelayAcknowledgements to complete",
				zap.String("src_chain_id", src.ChainID()),
				zap.String("src_channel_id", srcChannel.ChannelId),
				zap.String("dst_chain_id", dst.ChainID()),
				zap.String("dst_channel_id", srcChannel.Counterparty.ChannelId),
				zap.Error(ctx.Err()),
			)
			return false
		}

		// Otherwise, not a context error, but an application-level error.
		log.Warn(
			"Relay acknowledgements error",
			zap.String("src_chain_id", src.ChainID()),
			zap.String("src_channel_id", srcChannel.ChannelId),
			zap.String("dst_chain_id", dst.ChainID()),
			zap.String("dst_channel_id", srcChannel.Counterparty.ChannelId),
			zap.Error(err),
		)
		return false
	}
	elap_1 := time.Since(st_1)
	elap := time.Since(st)
	fmt.Println("relayUnrelayedAcks", elap)
	fmt.Println("UnrelayedAcknowledgements", elap_0, "RelayAcknowledgements", elap_1)
	return true
}