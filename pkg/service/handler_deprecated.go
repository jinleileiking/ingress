package service

import (
	"context"

	"google.golang.org/protobuf/proto"

	"github.com/livekit/ingress/pkg/config"
	"github.com/livekit/ingress/pkg/errors"
	"github.com/livekit/ingress/pkg/media"
	"github.com/livekit/protocol/ingress"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/tracer"
)

type HandlerV0 struct {
	conf      *config.Config
	rpcServer ingress.RPCServer
	kill      chan struct{}
}

func NewHandlerV0(conf *config.Config, rpcServer ingress.RPCServer) *HandlerV0 {
	return &HandlerV0{
		conf:      conf,
		rpcServer: rpcServer,
		kill:      make(chan struct{}),
	}
}

func (h *HandlerV0) HandleIngress(ctx context.Context, info *livekit.IngressInfo, wsUrl string, token string) {
	ctx, span := tracer.Start(ctx, "Handler.HandleRequest")
	defer span.End()

	p, err := h.buildPipeline(ctx, info, wsUrl, token)
	if err != nil {
		span.RecordError(err)
		return
	}

	// subscribe to request channel
	requests, err := h.rpcServer.IngressSubscription(context.Background(), p.GetInfo().IngressId)
	if err != nil {
		span.RecordError(err)
		return
	}
	defer func() {
		err := requests.Close()
		if err != nil {
			logger.Errorw("failed to unsubscribe from request channel", err)
		}
	}()

	// start ingress
	result := make(chan *livekit.IngressInfo, 1)
	go func() {
		result <- p.Run(ctx)
	}()

	for {
		select {
		case <-h.kill:
			// kill signal received
			p.SendEOS(ctx)

		case res := <-result:
			// recording finished
			h.sendUpdate(ctx, res)
			return

		case msg := <-requests.Channel():
			// request received
			request := &livekit.IngressRequest{}
			err = proto.Unmarshal(requests.Payload(msg), request)
			if err != nil {
				logger.Errorw("failed to read request", err, "ingressID", p.GetInfo().IngressId)
				continue
			}
			logger.Debugw("handling request", "ingressID", p.GetInfo().IngressId, "requestID", request.RequestId)

			p.SendEOS(ctx)
			h.sendResponse(ctx, request, p.GetInfo(), err)
		}
	}
}

func (h *HandlerV0) buildPipeline(ctx context.Context, info *livekit.IngressInfo, wsUrl string, token string) (*media.Pipeline, error) {
	ctx, span := tracer.Start(ctx, "Handler.buildPipeline")
	defer span.End()

	// build/verify params
	var p *media.Pipeline
	params, err := media.GetParams(ctx, h.conf, info, wsUrl, token)
	if err == nil {
		// create the pipeline
		p, err = media.New(ctx, h.conf, params)
	}

	if err != nil {
		if params != nil {
			info = params.IngressInfo
		}

		info.State.Error = err.Error()
		info.State.Status = livekit.IngressState_ENDPOINT_ERROR
		h.sendUpdate(ctx, info)
		return nil, err
	}

	p.OnStatusUpdate(h.sendUpdate)
	return p, nil
}

func (h *HandlerV0) sendUpdate(ctx context.Context, info *livekit.IngressInfo) {
	if info.State.Status == livekit.IngressState_ENDPOINT_ERROR {
		logger.Errorw("ingress failed", errors.New(info.State.Error))
	}

	if err := h.rpcServer.SendUpdate(ctx, info.IngressId, info.State); err != nil {
		logger.Errorw("failed to send update", err)
	}
}

func (h *HandlerV0) sendResponse(ctx context.Context, req *livekit.IngressRequest, info *livekit.IngressInfo, err error) {
	args := []interface{}{
		"ingressID", info.IngressId,
		"requestID", req.RequestId,
		"senderID", req.SenderId,
	}

	if err != nil {
		logger.Errorw("request failed", err, args...)
	} else {
		logger.Debugw("request handled", args...)
	}

	if err := h.rpcServer.SendResponse(ctx, req, info.State, err); err != nil {
		logger.Errorw("failed to send response", err, args...)
	}
}

func (h *HandlerV0) Kill() {
	select {
	case <-h.kill:
		return
	default:
		close(h.kill)
	}
}
