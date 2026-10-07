package topology

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/transport"
)

// HubHooks are the hub-core (WP05) callbacks composed with the registry by
// NodeServerConfig.
type HubHooks struct {
	// Welcome returns the hub's committed report watermark for the node and
	// the queued commands to resend (Seq > hello.LastCommandSeq). Required.
	Welcome func(ctx context.Context, nodeID string, hello contracts.NodeHello) (transport.WelcomeInfo, error)
	// OnReport commits durable reports (OriginSeq > 0) idempotently by
	// (node_id, origin_seq), and may also observe status reports after the
	// registry applied them. Required.
	OnReport func(ctx context.Context, nodeID string, r contracts.NodeReport) error
	// OnCommandAck records node command acknowledgements. Optional.
	OnCommandAck func(ctx context.Context, nodeID string, ack contracts.CommandAck) error
	// OnDisconnect runs after the registry recorded the disconnect. Optional.
	OnDisconnect func(ctx context.Context, nodeID string, info transport.DisconnectInfo)
}

// NodeServerConfig returns a transport.NodeServerConfig whose callbacks keep
// the registry current (ObserveHello on connect, ObserveStatus for status
// reports, ObserveDisconnect guarded by connection ID) and delegate durable
// state to the hub hooks. Set Auth/HubID/heartbeat fields on the result
// before calling transport.NewNodeServer.
func (s *Store) NodeServerConfig(h HubHooks) (transport.NodeServerConfig, error) {
	if h.Welcome == nil || h.OnReport == nil {
		return transport.NodeServerConfig{}, errors.New("topology: HubHooks requires Welcome and OnReport")
	}
	return transport.NodeServerConfig{
		Clock: s.clock,
		OnHello: func(ctx context.Context, nodeID string, hello contracts.NodeHello, info transport.ConnInfo) (transport.WelcomeInfo, error) {
			if _, err := s.ObserveHello(ctx, nodeID, info, hello); err != nil {
				return transport.WelcomeInfo{}, err
			}
			return h.Welcome(ctx, nodeID, hello)
		},
		OnReport: func(ctx context.Context, nodeID string, r contracts.NodeReport) error {
			if r.Kind == contracts.ReportStatus {
				var st contracts.NodeStatusReport
				if err := json.Unmarshal(r.Payload, &st); err != nil {
					return contracts.Errorf(contracts.ErrInvalid, "malformed status report")
				}
				if err := s.ObserveStatus(ctx, nodeID, st); err != nil {
					return err
				}
			}
			return h.OnReport(ctx, nodeID, r)
		},
		OnCommandAck: h.OnCommandAck,
		OnDisconnect: func(ctx context.Context, nodeID string, info transport.DisconnectInfo) {
			if !info.Replaced {
				_, _ = s.ObserveDisconnect(ctx, nodeID, info.ConnID, info.LastSeen)
			}
			if h.OnDisconnect != nil {
				h.OnDisconnect(ctx, nodeID, info)
			}
		},
	}, nil
}
