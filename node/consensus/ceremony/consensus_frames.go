package ceremony

import (
	"bytes"
	"context"
	"io"
	"time"

	"source.quilibrium.com/quilibrium/monorepo/node/config"

	"github.com/pkg/errors"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"source.quilibrium.com/quilibrium/monorepo/node/consensus"
	qcrypto "source.quilibrium.com/quilibrium/monorepo/node/crypto"
	"source.quilibrium.com/quilibrium/monorepo/node/execution/intrinsics/ceremony/application"
	"source.quilibrium.com/quilibrium/monorepo/node/p2p"
	"source.quilibrium.com/quilibrium/monorepo/node/protobufs"
)

func (e *CeremonyDataClockConsensusEngine) prove(
	previousFrame *protobufs.ClockFrame,
) (*protobufs.ClockFrame, error) {
	if !e.frameProverTrie.Contains(e.provingKeyAddress) {
		e.stagedLobbyStateTransitionsMx.Lock()
		e.stagedLobbyStateTransitions = &protobufs.CeremonyLobbyStateTransition{}
		e.stagedLobbyStateTransitionsMx.Unlock()

		e.state = consensus.EngineStateCollecting
		return previousFrame, nil
	}

	e.stagedLobbyStateTransitionsMx.Lock()
	executionOutput := &protobufs.IntrinsicExecutionOutput{}
	app, err := application.MaterializeApplicationFromFrame(previousFrame)
	if err != nil {
		e.stagedLobbyStateTransitions = &protobufs.CeremonyLobbyStateTransition{}
		e.stagedLobbyStateTransitionsMx.Unlock()
		return nil, errors.Wrap(err, "prove")
	}

	if e.stagedLobbyStateTransitions == nil {
		e.stagedLobbyStateTransitions = &protobufs.CeremonyLobbyStateTransition{}
	}

	e.logger.Info(
		"proving new frame",
		zap.Int("state_transitions", len(e.stagedLobbyStateTransitions.TypeUrls)),
	)

	var validLobbyTransitions *protobufs.CeremonyLobbyStateTransition
	var skippedTransition *protobufs.CeremonyLobbyStateTransition
	app, validLobbyTransitions, skippedTransition, err = app.ApplyTransition(
		previousFrame.FrameNumber,
		e.stagedLobbyStateTransitions,
		true,
	)
	if err != nil {
		e.stagedLobbyStateTransitions = &protobufs.CeremonyLobbyStateTransition{}
		e.stagedLobbyStateTransitionsMx.Unlock()
		return nil, errors.Wrap(err, "prove")
	}

	e.stagedLobbyStateTransitions = skippedTransition
	defer e.stagedLobbyStateTransitionsMx.Unlock()

	lobbyState, err := app.MaterializeLobbyStateFromApplication()
	if err != nil {
		return nil, errors.Wrap(err, "prove")
	}

	executionOutput.Address = application.CEREMONY_ADDRESS
	executionOutput.Output, err = proto.Marshal(lobbyState)
	if err != nil {
		return nil, errors.Wrap(err, "prove")
	}

	executionOutput.Proof, err = proto.Marshal(validLobbyTransitions)
	if err != nil {
		return nil, errors.Wrap(err, "prove")
	}

	data, err := proto.Marshal(executionOutput)
	if err != nil {
		return nil, errors.Wrap(err, "prove")
	}

	e.logger.Debug("encoded execution output")

	commitment, err := e.inclusionProver.Commit(
		data,
		protobufs.IntrinsicExecutionOutputType,
	)
	if err != nil {
		return nil, errors.Wrap(err, "prove")
	}

	e.logger.Debug("creating kzg proof")
	proof, err := e.inclusionProver.ProveAggregate(
		[]*qcrypto.InclusionCommitment{commitment},
	)
	if err != nil {
		return nil, errors.Wrap(err, "prove")
	}

	e.logger.Debug("finalizing execution proof")

	frame, err := e.frameProver.ProveDataClockFrame(
		previousFrame,
		[][]byte{proof.AggregateCommitment},
		[]*protobufs.InclusionAggregateProof{
			{
				Filter:      e.filter,
				FrameNumber: previousFrame.FrameNumber + 1,
				InclusionCommitments: []*protobufs.InclusionCommitment{
					{
						Filter:      e.filter,
						FrameNumber: previousFrame.FrameNumber + 1,
						TypeUrl:     proof.InclusionCommitments[0].TypeUrl,
						Commitment:  proof.InclusionCommitments[0].Commitment,
						Data:        data,
						Position:    0,
					},
				},
				Proof: proof.Proof,
			},
		},
		e.provingKey,
		time.Now().UnixMilli(),
		e.difficulty,
	)
	if err != nil {
		return nil, errors.Wrap(err, "prove")
	}
	e.logger.Info(
		"returning new proven frame",
		zap.Uint64("frame_number", frame.FrameNumber),
		zap.Int("proof_count", len(frame.AggregateProofs)),
		zap.Int("commitment_count", len(frame.Input[516:])/74),
	)
	return frame, nil
}

func (e *CeremonyDataClockConsensusEngine) GetMostAheadPeer() (
	[]byte,
	uint64,
	error,
) {
	frame, err := e.dataTimeReel.Head()
	if err != nil {
		panic(err)
	}

	e.logger.Info(
		"checking peer list",
		zap.Int("peers", len(e.peerMap)),
		zap.Int("uncooperative_peers", len(e.uncooperativePeersMap)),
		zap.Uint64("current_head_frame", frame.FrameNumber),
	)

	max := frame.FrameNumber
	var peer []byte = nil
	e.peerMapMx.RLock()
	for _, v := range e.peerMap {
		_, ok := e.uncooperativePeersMap[string(v.peerId)]
		if v.maxFrame > max &&
			v.timestamp > config.GetMinimumVersionCutoff().UnixMilli() &&
			bytes.Compare(v.version, config.GetMinimumVersion()) >= 0 && !ok {
			peer = v.peerId
			max = v.maxFrame
		}
	}
	e.peerMapMx.RUnlock()

	if peer == nil {
		return nil, 0, p2p.ErrNoPeersAvailable
	}

	return peer, max, nil
}

func (e *CeremonyDataClockConsensusEngine) sync(
	currentLatest *protobufs.ClockFrame,
	maxFrame uint64,
	peerId []byte,
) (*protobufs.ClockFrame, error) {
	latest := currentLatest
	e.logger.Info("polling peer for new frames", zap.Binary("peer_id", peerId))
	cc, err := e.pubSub.GetDirectChannel(peerId, "")
	if err != nil {
		e.logger.Error(
			"could not establish direct channel",
			zap.Error(err),
		)
		e.peerMapMx.Lock()
		if _, ok := e.peerMap[string(peerId)]; ok {
			e.uncooperativePeersMap[string(peerId)] = e.peerMap[string(peerId)]
			e.uncooperativePeersMap[string(peerId)].timestamp = time.Now().UnixMilli()
			delete(e.peerMap, string(peerId))
		}
		e.peerMapMx.Unlock()
		return latest, errors.Wrap(err, "sync")
	}

	client := protobufs.NewCeremonyServiceClient(cc)

	from := latest.FrameNumber
	if from == 0 {
		from = 1
	}

	rangeParentSelectors := []*protobufs.ClockFrameParentSelectors{}
	if from > 128 {
		rangeSubtract := uint64(16)
		for {
			if from <= rangeSubtract {
				break
			}

			parentNumber := from - uint64(rangeSubtract)
			rangeSubtract *= 2
			parent, _, err := e.clockStore.GetDataClockFrame(
				e.filter,
				parentNumber,
				true,
			)
			if err != nil {
				break
			}

			parentSelector, err := parent.GetSelector()
			if err != nil {
				panic(err)
			}

			rangeParentSelectors = append(
				rangeParentSelectors,
				&protobufs.ClockFrameParentSelectors{
					FrameNumber:    parentNumber,
					ParentSelector: parentSelector.FillBytes(make([]byte, 32)),
				},
			)
		}
	}

	s, err := client.NegotiateCompressedSyncFrames(
		context.Background(),
		grpc.MaxCallRecvMsgSize(600*1024*1024),
	)
	if err != nil {
		e.logger.Debug(
			"received error from peer",
			zap.Error(err),
		)
		e.peerMapMx.Lock()
		if _, ok := e.peerMap[string(peerId)]; ok {
			e.uncooperativePeersMap[string(peerId)] = e.peerMap[string(peerId)]
			e.uncooperativePeersMap[string(peerId)].timestamp = time.Now().UnixMilli()
			delete(e.peerMap, string(peerId))
		}
		e.peerMapMx.Unlock()
		return latest, errors.Wrap(err, "sync")
	}

	err = s.Send(&protobufs.CeremonyCompressedSyncRequestMessage{
		SyncMessage: &protobufs.CeremonyCompressedSyncRequestMessage_Preflight{
			Preflight: &protobufs.ClockFramesPreflight{
				RangeParentSelectors: rangeParentSelectors,
			},
		},
	})
	if err != nil {
		return latest, errors.Wrap(err, "sync")
	}

	syncMsg, err := s.Recv()
	if err != nil {
		return latest, errors.Wrap(err, "sync")
	}
	preflight, ok := syncMsg.
		SyncMessage.(*protobufs.CeremonyCompressedSyncResponseMessage_Preflight)
	if !ok {
		s.CloseSend()
		return latest, errors.Wrap(
			errors.New("preflight message invalid"),
			"sync",
		)
	}

	// loop through parent selectors, set found to first match, and if subsequent
	// matches fail to be found, cancel the search, start from 1.
	found := uint64(0)
	parentSelector := make([]byte, 32)

	for _, selector := range preflight.Preflight.RangeParentSelectors {
		match, err := e.clockStore.GetStagedDataClockFrame(
			e.filter,
			selector.FrameNumber,
			selector.ParentSelector,
			true,
		)
		if err != nil && found == 0 {
			continue
		}
		if err != nil && found != 0 {
			found = 1
			e.logger.Info("could not find interstitial frame, setting search to 1")
			break
		}
		if match != nil && found == 0 {
			found = match.FrameNumber
			parentSelector = match.ParentSelector
			break
		}
	}
	if found != 0 && !bytes.Equal(parentSelector, make([]byte, 32)) {
		check, err := e.clockStore.GetStagedDataClockFrame(
			e.filter,
			found,
			parentSelector,
			true,
		)
		if err != nil {
			from = 1
		} else {
			e.logger.Info("checking interstitial continuity")
			for check.FrameNumber > 1 {
				check, err = e.clockStore.GetStagedDataClockFrame(
					e.filter,
					check.FrameNumber-1,
					check.ParentSelector,
					true,
				)
				if err != nil {
					from = 1
					e.logger.Info(
						"could not confirm interstitial continuity, setting search to 1",
					)
					break
				}
			}
			from = found
		}
	}

	err = s.Send(&protobufs.CeremonyCompressedSyncRequestMessage{
		SyncMessage: &protobufs.CeremonyCompressedSyncRequestMessage_Request{
			Request: &protobufs.ClockFramesRequest{
				Filter:          e.filter,
				FromFrameNumber: from,
				ToFrameNumber:   0,
				ParentSelector:  parentSelector,
			},
		},
	})
	if err != nil {
		return latest, errors.Wrap(err, "sync")
	}

	for syncMsg, err = s.Recv(); err == nil; syncMsg, err = s.Recv() {
		sync, ok := syncMsg.
			SyncMessage.(*protobufs.CeremonyCompressedSyncResponseMessage_Response)
		if !ok {
			return latest, errors.Wrap(
				errors.New("response message invalid"),
				"sync",
			)
		}

		response := sync.Response

		e.logger.Info(
			"received compressed sync frame",
			zap.Uint64("from", response.FromFrameNumber),
			zap.Uint64("to", response.ToFrameNumber),
			zap.Int("frames", len(response.TruncatedClockFrames)),
			zap.Int("proofs", len(response.Proofs)),
		)

		// This can only happen if we get a peer with state that was initially
		// farther ahead, but something happened. However, this has a sticking
		// effect that doesn't go away for them until they're caught up again,
		// so let's not penalize their score and make everyone else suffer,
		// let's just move on:
		if response.FromFrameNumber == 0 && response.ToFrameNumber == 0 {
			if err := cc.Close(); err != nil {
				e.logger.Error("error while closing connection", zap.Error(err))
			}

			return currentLatest, errors.Wrap(ErrNoNewFrames, "sync")
		}

		var next *protobufs.ClockFrame
		if next, err = e.decompressAndStoreCandidates(
			peerId,
			response,
		); err != nil && !errors.Is(err, ErrNoNewFrames) {
			e.logger.Error(
				"could not decompress and store candidate",
				zap.Error(err),
			)
			e.peerMapMx.Lock()
			if _, ok := e.peerMap[string(peerId)]; ok {
				e.uncooperativePeersMap[string(peerId)] = e.peerMap[string(peerId)]
				e.uncooperativePeersMap[string(peerId)].timestamp = time.Now().
					UnixMilli()
				delete(e.peerMap, string(peerId))
			}
			e.peerMapMx.Unlock()

			if err := cc.Close(); err != nil {
				e.logger.Error("error while closing connection", zap.Error(err))
			}

			return currentLatest, errors.Wrap(err, "sync")
		}
		if next != nil {
			latest = next
		}
	}
	if err != nil && err != io.EOF && !errors.Is(err, ErrNoNewFrames) {
		e.logger.Debug("error while receiving sync", zap.Error(err))

		if err := cc.Close(); err != nil {
			e.logger.Error("error while closing connection", zap.Error(err))
		}

		e.peerMapMx.Lock()
		if _, ok := e.peerMap[string(peerId)]; ok {
			e.uncooperativePeersMap[string(peerId)] = e.peerMap[string(peerId)]
			e.uncooperativePeersMap[string(peerId)].timestamp = time.Now().UnixMilli()
			delete(e.peerMap, string(peerId))
		}
		e.peerMapMx.Unlock()

		return latest, errors.Wrap(err, "sync")
	}

	e.logger.Info(
		"received new leading frame",
		zap.Uint64("frame_number", latest.FrameNumber),
	)
	if err := cc.Close(); err != nil {
		e.logger.Error("error while closing connection", zap.Error(err))
	}

	e.dataTimeReel.Insert(latest, false)

	return latest, nil
}

func (e *CeremonyDataClockConsensusEngine) collect(
	currentFramePublished *protobufs.ClockFrame,
) (*protobufs.ClockFrame, error) {
	e.logger.Info("collecting vdf proofs")

	latest := currentFramePublished

	// With the increase of network size, constrain down to top thirty
	for i := 0; i < 30; i++ {
		peerId, maxFrame, err := e.GetMostAheadPeer()
		if err != nil {
			e.logger.Warn("no peers available, skipping sync")
			break
		} else if peerId == nil {
			e.logger.Info("currently up to date, skipping sync")
			break
		} else if maxFrame-2 > latest.FrameNumber {
			e.syncingStatus = SyncStatusSynchronizing
			latest, err = e.sync(latest, maxFrame, peerId)
			if err == nil {
				break
			}
		}
	}

	e.syncingStatus = SyncStatusNotSyncing

	if latest.FrameNumber < currentFramePublished.FrameNumber {
		latest = currentFramePublished
	}

	e.logger.Info(
		"returning leader frame",
		zap.Uint64("frame_number", latest.FrameNumber),
	)

	return latest, nil
}
