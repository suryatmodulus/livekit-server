// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package service

import (
	"context"
	"strconv"
	"time"

	"github.com/pkg/errors"
	"github.com/twitchtv/twirp"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
)

// A rooms service that supports a single node
type RoomService struct {
	roomConf          config.RoomConfig
	apiConf           config.APIConfig
	psrpcConf         rpc.PSRPCConfig
	router            routing.MessageRouter
	roomAllocator     RoomAllocator
	roomStore         ServiceStore
	agentClient       rtc.AgentClient
	egressLauncher    rtc.EgressLauncher
	topicFormatter    rpc.TopicFormatter
	roomClient        rpc.TypedRoomClient
	participantClient rpc.TypedParticipantClient
}

func NewRoomService(
	roomConf config.RoomConfig,
	apiConf config.APIConfig,
	psrpcConf rpc.PSRPCConfig,
	router routing.MessageRouter,
	roomAllocator RoomAllocator,
	serviceStore ServiceStore,
	agentClient rtc.AgentClient,
	egressLauncher rtc.EgressLauncher,
	topicFormatter rpc.TopicFormatter,
	roomClient rpc.TypedRoomClient,
	participantClient rpc.TypedParticipantClient,
) (svc *RoomService, err error) {
	svc = &RoomService{
		roomConf:          roomConf,
		apiConf:           apiConf,
		psrpcConf:         psrpcConf,
		router:            router,
		roomAllocator:     roomAllocator,
		roomStore:         serviceStore,
		agentClient:       agentClient,
		egressLauncher:    egressLauncher,
		topicFormatter:    topicFormatter,
		roomClient:        roomClient,
		participantClient: participantClient,
	}
	return
}

func (s *RoomService) CreateRoom(ctx context.Context, req *livekit.CreateRoomRequest) (*livekit.Room, error) {
	AppendLogFields(ctx, "room", req.Name, "request", req)
	if err := EnsureCreatePermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	} else if req.Egress != nil && s.egressLauncher == nil {
		return nil, ErrEgressNotConnected
	}

	rm, created, err := s.roomAllocator.CreateRoom(ctx, req)
	if err != nil {
		err = errors.Wrap(err, "could not create room")
		return nil, err
	}

	// actually start the room on an RTC node, to ensure metadata & empty timeout functionality
	res, err := s.router.StartParticipantSignal(ctx,
		livekit.RoomName(req.Name),
		routing.ParticipantInit{},
	)
	if err != nil {
		return nil, err
	}
	defer res.RequestSink.Close()
	defer res.ResponseSource.Close()

	// ensure it's created correctly
	err = s.confirmExecution(func() error {
		_, _, err := s.roomStore.LoadRoom(ctx, livekit.RoomName(req.Name), false)
		if err != nil {
			return ErrOperationFailed
		} else {
			return nil
		}
	})
	if err != nil {
		return nil, err
	}

	if created {
		go func() {
			s.agentClient.JobRequest(ctx, &livekit.Job{
				Id:   utils.NewGuid("JR_"),
				Type: livekit.JobType_JT_ROOM,
				Room: rm,
			})
		}()

		if req.Egress != nil && req.Egress.Room != nil {
			_, err = s.egressLauncher.StartEgress(ctx, &rpc.StartEgressRequest{
				Request: &rpc.StartEgressRequest_RoomComposite{
					RoomComposite: req.Egress.Room,
				},
				RoomId: rm.Sid,
			})
			if err != nil {
				return nil, err
			}
		}
	}

	return rm, nil
}

func (s *RoomService) ListRooms(ctx context.Context, req *livekit.ListRoomsRequest) (*livekit.ListRoomsResponse, error) {
	AppendLogFields(ctx, "room", req.Names)
	err := EnsureListPermission(ctx)
	if err != nil {
		return nil, twirpAuthError(err)
	}

	var names []livekit.RoomName
	if len(req.Names) > 0 {
		names = livekit.StringsAsIDs[livekit.RoomName](req.Names)
	}
	rooms, err := s.roomStore.ListRooms(ctx, names)
	if err != nil {
		// TODO: translate error codes to Twirp
		return nil, err
	}

	res := &livekit.ListRoomsResponse{
		Rooms: rooms,
	}
	return res, nil
}

func (s *RoomService) DeleteRoom(ctx context.Context, req *livekit.DeleteRoomRequest) (*livekit.DeleteRoomResponse, error) {
	AppendLogFields(ctx, "room", req.Room)
	if err := EnsureCreatePermission(ctx); err != nil {
		return nil, twirpAuthError(err)
	}

	return s.roomClient.DeleteRoom(ctx, s.topicFormatter.RoomTopic(ctx, livekit.RoomName(req.Room)), req)
}

func (s *RoomService) ListParticipants(ctx context.Context, req *livekit.ListParticipantsRequest) (*livekit.ListParticipantsResponse, error) {
	AppendLogFields(ctx, "room", req.Room)
	if err := EnsureAdminPermission(ctx, livekit.RoomName(req.Room)); err != nil {
		return nil, twirpAuthError(err)
	}

	participants, err := s.roomStore.ListParticipants(ctx, livekit.RoomName(req.Room))
	if err != nil {
		return nil, err
	}

	res := &livekit.ListParticipantsResponse{
		Participants: participants,
	}
	return res, nil
}

func (s *RoomService) GetParticipant(ctx context.Context, req *livekit.RoomParticipantIdentity) (*livekit.ParticipantInfo, error) {
	AppendLogFields(ctx, "room", req.Room, "participant", req.Identity)
	if err := EnsureAdminPermission(ctx, livekit.RoomName(req.Room)); err != nil {
		return nil, twirpAuthError(err)
	}

	participant, err := s.roomStore.LoadParticipant(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity))
	if err != nil {
		return nil, err
	}

	return participant, nil
}

func (s *RoomService) RemoveParticipant(ctx context.Context, req *livekit.RoomParticipantIdentity) (*livekit.RemoveParticipantResponse, error) {
	AppendLogFields(ctx, "room", req.Room, "participant", req.Identity)

	if err := EnsureAdminPermission(ctx, livekit.RoomName(req.Room)); err != nil {
		return nil, twirpAuthError(err)
	}

	if _, err := s.roomStore.LoadParticipant(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity)); err == ErrParticipantNotFound {
		return nil, twirp.NotFoundError("participant not found")
	}

	return s.participantClient.RemoveParticipant(ctx, s.topicFormatter.ParticipantTopic(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity)), req)
}

func (s *RoomService) MutePublishedTrack(ctx context.Context, req *livekit.MuteRoomTrackRequest) (*livekit.MuteRoomTrackResponse, error) {
	AppendLogFields(ctx, "room", req.Room, "participant", req.Identity, "trackID", req.TrackSid, "muted", req.Muted)
	if err := EnsureAdminPermission(ctx, livekit.RoomName(req.Room)); err != nil {
		return nil, twirpAuthError(err)
	}

	return s.participantClient.MutePublishedTrack(ctx, s.topicFormatter.ParticipantTopic(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity)), req)
}

func (s *RoomService) UpdateParticipant(ctx context.Context, req *livekit.UpdateParticipantRequest) (*livekit.ParticipantInfo, error) {
	AppendLogFields(ctx, "room", req.Room, "participant", req.Identity)
	maxMetadataSize := int(s.roomConf.MaxMetadataSize)
	if maxMetadataSize > 0 && len(req.Metadata) > maxMetadataSize {
		return nil, twirp.InvalidArgumentError(ErrMetadataExceedsLimits.Error(), strconv.Itoa(maxMetadataSize))
	}

	if err := EnsureAdminPermission(ctx, livekit.RoomName(req.Room)); err != nil {
		return nil, twirpAuthError(err)
	}

	return s.participantClient.UpdateParticipant(ctx, s.topicFormatter.ParticipantTopic(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity)), req)
}

func (s *RoomService) UpdateSubscriptions(ctx context.Context, req *livekit.UpdateSubscriptionsRequest) (*livekit.UpdateSubscriptionsResponse, error) {
	trackSIDs := append(make([]string, 0), req.TrackSids...)
	for _, pt := range req.ParticipantTracks {
		trackSIDs = append(trackSIDs, pt.TrackSids...)
	}
	AppendLogFields(ctx, "room", req.Room, "participant", req.Identity, "trackID", trackSIDs)

	if err := EnsureAdminPermission(ctx, livekit.RoomName(req.Room)); err != nil {
		return nil, twirpAuthError(err)
	}

	return s.participantClient.UpdateSubscriptions(ctx, s.topicFormatter.ParticipantTopic(ctx, livekit.RoomName(req.Room), livekit.ParticipantIdentity(req.Identity)), req)
}

func (s *RoomService) SendData(ctx context.Context, req *livekit.SendDataRequest) (*livekit.SendDataResponse, error) {
	roomName := livekit.RoomName(req.Room)
	AppendLogFields(ctx, "room", roomName, "size", len(req.Data))
	if err := EnsureAdminPermission(ctx, roomName); err != nil {
		return nil, twirpAuthError(err)
	}

	return s.roomClient.SendData(ctx, s.topicFormatter.RoomTopic(ctx, livekit.RoomName(req.Room)), req)
}

func (s *RoomService) UpdateRoomMetadata(ctx context.Context, req *livekit.UpdateRoomMetadataRequest) (*livekit.Room, error) {
	AppendLogFields(ctx, "room", req.Room, "size", len(req.Metadata))
	maxMetadataSize := int(s.roomConf.MaxMetadataSize)
	if maxMetadataSize > 0 && len(req.Metadata) > maxMetadataSize {
		return nil, twirp.InvalidArgumentError(ErrMetadataExceedsLimits.Error(), strconv.Itoa(maxMetadataSize))
	}

	if err := EnsureAdminPermission(ctx, livekit.RoomName(req.Room)); err != nil {
		return nil, twirpAuthError(err)
	}

	room, _, err := s.roomStore.LoadRoom(ctx, livekit.RoomName(req.Room), false)
	if err != nil {
		return nil, err
	}

	// no one has joined the room, would not have been created on an RTC node.
	// in this case, we'd want to run create again
	room, created, err := s.roomAllocator.CreateRoom(ctx, &livekit.CreateRoomRequest{
		Name:     req.Room,
		Metadata: req.Metadata,
	})
	if err != nil {
		return nil, err
	}

	_, err = s.roomClient.UpdateRoomMetadata(ctx, s.topicFormatter.RoomTopic(ctx, livekit.RoomName(req.Room)), req)
	if err != nil {
		return nil, err
	}

	err = s.confirmExecution(func() error {
		room, _, err = s.roomStore.LoadRoom(ctx, livekit.RoomName(req.Room), false)
		if err != nil {
			return err
		}
		if room.Metadata != req.Metadata {
			return ErrOperationFailed
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if created {
		go func() {
			s.agentClient.JobRequest(ctx, &livekit.Job{
				Id:   utils.NewGuid("JR_"),
				Type: livekit.JobType_JT_ROOM,
				Room: room,
			})
		}()
	}

	return room, nil
}

func (s *RoomService) confirmExecution(f func() error) error {
	expired := time.After(s.apiConf.ExecutionTimeout)
	var err error
	for {
		select {
		case <-expired:
			return err
		default:
			err = f()
			if err == nil {
				return nil
			}
			time.Sleep(s.apiConf.CheckInterval)
		}
	}
}
