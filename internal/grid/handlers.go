// Copyright (c) 2015-2023 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package grid

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/minio/minio/internal/hash/sha256"
	"github.com/minio/minio/internal/logger"
	"github.com/tinylib/msgp/msgp"
)

//go:generate stringer -type=HandlerID -output=handlers_string.go -trimprefix=Handler msg.go $GOFILE

// HandlerID is a handler identifier.
// It is used to determine request routing on the server.
// Handlers can be registered with a static subroute.
const (
	// handlerInvalid is reserved to check for uninitialized values.
	handlerInvalid HandlerID = iota
	HandlerLockLock
	HandlerLockRLock
	HandlerLockUnlock
	HandlerLockRUnlock
	HandlerLockRefresh
	HandlerLockForceUnlock
	HandlerWalkDir
	HandlerStatVol
	HandlerDiskInfo
	HandlerNSScanner
	HandlerReadXL
	HandlerReadVersion
	HandlerDeleteFile
	HandlerDeleteVersion
	HandlerUpdateMetadata
	HandlerWriteMetadata
	HandlerCheckParts
	HandlerRenameData

	HandlerServerVerify
	// Add more above here ^^^
	// If all handlers are used, the type of Handler can be changed.
	// Handlers have no versioning, so non-compatible handler changes must result in new IDs.
	handlerTest
	handlerTest2
	handlerLast
)

// handlerPrefixes are prefixes for handler IDs used for tracing.
// If a handler is not listed here, it will be traced with "grid" prefix.
var handlerPrefixes = [handlerLast]string{
	HandlerLockLock:        lockPrefix,
	HandlerLockRLock:       lockPrefix,
	HandlerLockUnlock:      lockPrefix,
	HandlerLockRUnlock:     lockPrefix,
	HandlerLockRefresh:     lockPrefix,
	HandlerLockForceUnlock: lockPrefix,
	HandlerWalkDir:         storagePrefix,
	HandlerStatVol:         storagePrefix,
	HandlerDiskInfo:        storagePrefix,
	HandlerNSScanner:       storagePrefix,
	HandlerReadXL:          storagePrefix,
	HandlerReadVersion:     storagePrefix,
	HandlerDeleteFile:      storagePrefix,
	HandlerDeleteVersion:   storagePrefix,
	HandlerUpdateMetadata:  storagePrefix,
	HandlerWriteMetadata:   storagePrefix,
	HandlerCheckParts:      storagePrefix,
	HandlerRenameData:      storagePrefix,
	HandlerServerVerify:    bootstrapPrefix,
}

const (
	lockPrefix      = "lockR"
	storagePrefix   = "storageR"
	bootstrapPrefix = "bootstrap"
)

func init() {
	// Static check if we exceed 255 handler ids.
	// Extend the type to uint16 when hit.
	if handlerLast > 255 {
		panic(fmt.Sprintf("out of handler IDs. %d > %d", handlerLast, 255))
	}
}

func (h HandlerID) valid() bool {
	return h != handlerInvalid && h < handlerLast
}

func (h HandlerID) isTestHandler() bool {
	return h >= handlerTest && h <= handlerTest2
}

// RemoteErr is a remote error type.
// Any error seen on a remote will be returned like this.
type RemoteErr string

// NewRemoteErr creates a new remote error.
// The error type is not preserved.
func NewRemoteErr(err error) *RemoteErr {
	if err == nil {
		return nil
	}
	r := RemoteErr(err.Error())
	return &r
}

// NewRemoteErrf creates a new remote error from a format string.
func NewRemoteErrf(format string, a ...any) *RemoteErr {
	r := RemoteErr(fmt.Sprintf(format, a...))
	return &r
}

// NewNPErr is a helper to no payload and optional remote error.
// The error type is not preserved.
func NewNPErr(err error) (NoPayload, *RemoteErr) {
	if err == nil {
		return NoPayload{}, nil
	}
	r := RemoteErr(err.Error())
	return NoPayload{}, &r
}

// NewRemoteErrString creates a new remote error from a string.
func NewRemoteErrString(msg string) *RemoteErr {
	r := RemoteErr(msg)
	return &r
}

func (r RemoteErr) Error() string {
	return string(r)
}

// Is returns if the string representation matches.
func (r *RemoteErr) Is(other error) bool {
	if r == nil || other == nil {
		return r == other
	}
	var o RemoteErr
	if errors.As(other, &o) {
		return r == &o
	}
	return false
}

// IsRemoteErr returns the value if the error is a RemoteErr.
func IsRemoteErr(err error) *RemoteErr {
	var r RemoteErr
	if errors.As(err, &r) {
		return &r
	}
	return nil
}

type (
	// SingleHandlerFn is handlers for one to one requests.
	// A non-nil error value will be returned as RemoteErr(msg) to client.
	// No client information or cancellation (deadline) is available.
	// Include this in payload if needed.
	// Payload should be recycled with PutByteBuffer if not needed after the call.
	SingleHandlerFn func(payload []byte) ([]byte, *RemoteErr)

	// StatelessHandlerFn must handle incoming stateless request.
	// A non-nil error value will be returned as RemoteErr(msg) to client.
	StatelessHandlerFn func(ctx context.Context, payload []byte, resp chan<- []byte) *RemoteErr

	// StatelessHandler is handlers for one to many requests,
	// where responses may be dropped.
	// Stateless requests provide no incoming stream and there is no flow control
	// on outgoing messages.
	StatelessHandler struct {
		Handle StatelessHandlerFn
		// OutCapacity is the output capacity on the caller.
		// If <= 0 capacity will be 1.
		OutCapacity int
	}

	// StreamHandlerFn must process a request with an optional initial payload.
	// It must keep consuming from 'in' until it returns.
	// 'in' and 'out' are independent.
	// The handler should never close out.
	// Buffers received from 'in'  can be recycled with PutByteBuffer.
	// Buffers sent on out can not be referenced once sent.
	StreamHandlerFn func(ctx context.Context, payload []byte, in <-chan []byte, out chan<- []byte) *RemoteErr

	// StreamHandler handles fully bidirectional streams,
	// There is flow control in both directions.
	StreamHandler struct {
		// Handle an incoming request. Initial payload is sent.
		// Additional input packets (if any) are streamed to request.
		// Upstream will block when request channel is full.
		// Response packets can be sent at any time.
		// Any non-nil error sent as response means no more responses are sent.
		Handle StreamHandlerFn

		// Subroute for handler.
		// Subroute must be static and clients should specify a matching subroute.
		// Should not be set unless there are different handlers for the same HandlerID.
		Subroute string

		// OutCapacity is the output capacity. If <= 0 capacity will be 1.
		OutCapacity int

		// InCapacity is the output capacity.
		// If == 0 no input is expected
		InCapacity int
	}
)

type subHandlerID [32]byte

func makeSubHandlerID(id HandlerID, subRoute string) subHandlerID {
	b := subHandlerID(sha256.Sum256([]byte(subRoute)))
	b[0] = byte(id)
	b[1] = 0 // Reserved
	return b
}

func (s subHandlerID) withHandler(id HandlerID) subHandlerID {
	s[0] = byte(id)
	s[1] = 0 // Reserved
	return s
}

func (s *subHandlerID) String() string {
	if s == nil {
		return ""
	}
	return hex.EncodeToString(s[:])
}

func makeZeroSubHandlerID(id HandlerID) subHandlerID {
	return subHandlerID{byte(id)}
}

type handlers struct {
	single    [handlerLast]SingleHandlerFn
	stateless [handlerLast]*StatelessHandler
	streams   [handlerLast]*StreamHandler

	subSingle    map[subHandlerID]SingleHandlerFn
	subStateless map[subHandlerID]*StatelessHandler
	subStreams   map[subHandlerID]*StreamHandler
}

func (h *handlers) init() {
	h.subSingle = make(map[subHandlerID]SingleHandlerFn)
	h.subStateless = make(map[subHandlerID]*StatelessHandler)
	h.subStreams = make(map[subHandlerID]*StreamHandler)
}

func (h *handlers) hasAny(id HandlerID) bool {
	if !id.valid() {
		return false
	}
	return h.single[id] != nil || h.stateless[id] != nil || h.streams[id] != nil
}

func (h *handlers) hasSubhandler(id subHandlerID) bool {
	return h.subSingle[id] != nil || h.subStateless[id] != nil || h.subStreams[id] != nil
}

// RoundTripper provides an interface for type roundtrip serialization.
type RoundTripper interface {
	msgp.Unmarshaler
	msgp.Marshaler
	msgp.Sizer

	comparable
}

// SingleHandler is a type safe handler for single roundtrip requests.
type SingleHandler[Req, Resp RoundTripper] struct {
	id             HandlerID
	sharedResponse bool

	reqPool  sync.Pool
	respPool sync.Pool

	nilReq  Req
	nilResp Resp
}

// NewSingleHandler creates a typed handler that can provide Marshal/Unmarshal.
// Use Register to register a server handler.
// Use Call to initiate a clientside call.
func NewSingleHandler[Req, Resp RoundTripper](h HandlerID, newReq func() Req, newResp func() Resp) *SingleHandler[Req, Resp] {
	s := SingleHandler[Req, Resp]{id: h}
	s.reqPool.New = func() interface{} {
		return newReq()
	}
	s.respPool.New = func() interface{} {
		return newResp()
	}
	return &s
}

// PutResponse will accept a response for reuse.
// These should be returned by the caller.
func (h *SingleHandler[Req, Resp]) PutResponse(r Resp) {
	if r != h.nilResp {
		h.respPool.Put(r)
	}
}

// WithSharedResponse indicates it is unsafe to reuse the response.
// Typically this is used when the response sharing part of its data structure.
func (h *SingleHandler[Req, Resp]) WithSharedResponse() *SingleHandler[Req, Resp] {
	h.sharedResponse = true
	return h
}

// NewResponse creates a new response.
// Handlers can use this to create a reusable response.
// The response may be reused, so caller should clear any fields.
func (h *SingleHandler[Req, Resp]) NewResponse() Resp {
	return h.respPool.Get().(Resp)
}

// putRequest will accept a request for reuse.
// This is not exported, since it shouldn't be needed.
func (h *SingleHandler[Req, Resp]) putRequest(r Req) {
	if r != h.nilReq {
		h.reqPool.Put(r)
	}
}

// NewRequest creates a new request.
// Handlers can use this to create a reusable request.
// The request may be reused, so caller should clear any fields.
func (h *SingleHandler[Req, Resp]) NewRequest() Req {
	return h.reqPool.Get().(Req)
}

// Register a handler for a Req -> Resp roundtrip.
func (h *SingleHandler[Req, Resp]) Register(m *Manager, handle func(req Req) (resp Resp, err *RemoteErr), subroute ...string) error {
	return m.RegisterSingleHandler(h.id, func(payload []byte) ([]byte, *RemoteErr) {
		req := h.NewRequest()
		_, err := req.UnmarshalMsg(payload)
		if err != nil {
			PutByteBuffer(payload)
			r := RemoteErr(err.Error())
			return nil, &r
		}
		resp, rerr := handle(req)
		h.putRequest(req)
		if rerr != nil {
			PutByteBuffer(payload)
			return nil, rerr
		}
		payload, err = resp.MarshalMsg(payload[:0])
		if !h.sharedResponse {
			h.PutResponse(resp)
		}
		if err != nil {
			PutByteBuffer(payload)
			r := RemoteErr(err.Error())
			return nil, &r
		}
		return payload, nil
	}, subroute...)
}

// Requester is able to send requests to a remote.
type Requester interface {
	Request(ctx context.Context, h HandlerID, req []byte) ([]byte, error)
}

// Call the remote with the request and return the response.
// The response should be returned with PutResponse when no error.
// If no deadline is set, a 1-minute deadline is added.
func (h *SingleHandler[Req, Resp]) Call(ctx context.Context, c Requester, req Req) (resp Resp, err error) {
	payload, err := req.MarshalMsg(GetByteBuffer()[:0])
	if err != nil {
		return resp, err
	}
	ctx = context.WithValue(ctx, TraceParamsKey{}, req)
	res, err := c.Request(ctx, h.id, payload)
	PutByteBuffer(payload)
	if err != nil {
		return resp, err
	}
	r := h.NewResponse()
	_, err = r.UnmarshalMsg(res)
	if err != nil {
		h.PutResponse(r)
		return resp, err
	}
	PutByteBuffer(res)
	return r, err
}

// RemoteClient contains information about the caller.
type RemoteClient struct {
	Name string
}

type (
	ctxCallerKey   = struct{}
	ctxSubrouteKey = struct{}
)

// GetCaller returns caller information from contexts provided to handlers.
func GetCaller(ctx context.Context) *RemoteClient {
	val, _ := ctx.Value(ctxCallerKey{}).(*RemoteClient)
	return val
}

// GetSubroute returns caller information from contexts provided to handlers.
func GetSubroute(ctx context.Context) string {
	val, _ := ctx.Value(ctxSubrouteKey{}).(string)
	return val
}

func setCaller(ctx context.Context, cl *RemoteClient) context.Context {
	return context.WithValue(ctx, ctxCallerKey{}, cl)
}

func setSubroute(ctx context.Context, s string) context.Context {
	return context.WithValue(ctx, ctxSubrouteKey{}, s)
}

// StreamTypeHandler is a type safe handler for streaming requests.
type StreamTypeHandler[Payload, Req, Resp RoundTripper] struct {
	WithPayload bool

	// Override the default capacities (1)
	OutCapacity int

	// Set to 0 if no input is expected.
	// Will be 0 if newReq is nil.
	InCapacity int

	reqPool        sync.Pool
	respPool       sync.Pool
	id             HandlerID
	newPayload     func() Payload
	nilReq         Req
	nilResp        Resp
	sharedResponse bool
}

// NewStream creates a typed handler that can provide Marshal/Unmarshal.
// Use Register to register a server handler.
// Use Call to initiate a clientside call.
// newPayload can be nil. In that case payloads will always be nil.
// newReq can be nil. In that case no input stream is expected and the handler will be called with nil 'in' channel.
func NewStream[Payload, Req, Resp RoundTripper](h HandlerID, newPayload func() Payload, newReq func() Req, newResp func() Resp) *StreamTypeHandler[Payload, Req, Resp] {
	if newResp == nil {
		panic("newResp missing in NewStream")
	}

	s := newStreamHandler[Payload, Req, Resp](h)
	if newReq != nil {
		s.reqPool.New = func() interface{} {
			return newReq()
		}
	} else {
		s.InCapacity = 0
	}
	s.respPool.New = func() interface{} {
		return newResp()
	}
	s.newPayload = newPayload
	s.WithPayload = newPayload != nil
	return s
}

// WithSharedResponse indicates it is unsafe to reuse the response.
// Typically this is used when the response sharing part of its data structure.
func (h *StreamTypeHandler[Payload, Req, Resp]) WithSharedResponse() *StreamTypeHandler[Payload, Req, Resp] {
	h.sharedResponse = true
	return h
}

// NewPayload creates a new payload.
func (h *StreamTypeHandler[Payload, Req, Resp]) NewPayload() Payload {
	return h.newPayload()
}

// NewRequest creates a new request.
// The struct may be reused, so caller should clear any fields.
func (h *StreamTypeHandler[Payload, Req, Resp]) NewRequest() Req {
	return h.reqPool.Get().(Req)
}

// PutRequest will accept a request for reuse.
// These should be returned by the handler.
func (h *StreamTypeHandler[Payload, Req, Resp]) PutRequest(r Req) {
	if r != h.nilReq {
		h.reqPool.Put(r)
	}
}

// PutResponse will accept a response for reuse.
// These should be returned by the caller.
func (h *StreamTypeHandler[Payload, Req, Resp]) PutResponse(r Resp) {
	if r != h.nilResp {
		h.respPool.Put(r)
	}
}

// NewResponse creates a new response.
// Handlers can use this to create a reusable response.
func (h *StreamTypeHandler[Payload, Req, Resp]) NewResponse() Resp {
	return h.respPool.Get().(Resp)
}

func newStreamHandler[Payload, Req, Resp RoundTripper](h HandlerID) *StreamTypeHandler[Payload, Req, Resp] {
	return &StreamTypeHandler[Payload, Req, Resp]{id: h, InCapacity: 1, OutCapacity: 1}
}

// Register a handler for two-way streaming with payload, input stream and output stream.
// An optional subroute can be given. Multiple entries are joined with '/'.
func (h *StreamTypeHandler[Payload, Req, Resp]) Register(m *Manager, handle func(ctx context.Context, p Payload, in <-chan Req, out chan<- Resp) *RemoteErr, subroute ...string) error {
	return h.register(m, handle, subroute...)
}

// RegisterNoInput a handler for one-way streaming with payload and output stream.
// An optional subroute can be given. Multiple entries are joined with '/'.
func (h *StreamTypeHandler[Payload, Req, Resp]) RegisterNoInput(m *Manager, handle func(ctx context.Context, p Payload, out chan<- Resp) *RemoteErr, subroute ...string) error {
	h.InCapacity = 0
	return h.register(m, func(ctx context.Context, p Payload, in <-chan Req, out chan<- Resp) *RemoteErr {
		return handle(ctx, p, out)
	}, subroute...)
}

// RegisterNoPayload a handler for one-way streaming with payload and output stream.
// An optional subroute can be given. Multiple entries are joined with '/'.
func (h *StreamTypeHandler[Payload, Req, Resp]) RegisterNoPayload(m *Manager, handle func(ctx context.Context, in <-chan Req, out chan<- Resp) *RemoteErr, subroute ...string) error {
	h.WithPayload = false
	return h.register(m, func(ctx context.Context, p Payload, in <-chan Req, out chan<- Resp) *RemoteErr {
		return handle(ctx, in, out)
	}, subroute...)
}

// Register a handler for two-way streaming with optional payload and input stream.
func (h *StreamTypeHandler[Payload, Req, Resp]) register(m *Manager, handle func(ctx context.Context, p Payload, in <-chan Req, out chan<- Resp) *RemoteErr, subroute ...string) error {
	return m.RegisterStreamingHandler(h.id, StreamHandler{
		Handle: func(ctx context.Context, payload []byte, in <-chan []byte, out chan<- []byte) *RemoteErr {
			var plT Payload
			if h.WithPayload {
				plT = h.NewPayload()
				_, err := plT.UnmarshalMsg(payload)
				PutByteBuffer(payload)
				if err != nil {
					r := RemoteErr(err.Error())
					return &r
				}
			}

			var inT chan Req
			if h.InCapacity > 0 {
				// Don't add extra buffering
				inT = make(chan Req)
				go func() {
					defer close(inT)
					for {
						select {
						case <-ctx.Done():
							return
						case v, ok := <-in:
							if !ok {
								return
							}
							input := h.NewRequest()
							_, err := input.UnmarshalMsg(v)
							if err != nil {
								logger.LogOnceIf(ctx, err, err.Error())
							}
							PutByteBuffer(v)
							// Send input
							select {
							case <-ctx.Done():
								return
							case inT <- input:
							}
						}
					}
				}()
			}
			outT := make(chan Resp)
			outDone := make(chan struct{})
			go func() {
				defer close(outDone)
				dropOutput := false
				for v := range outT {
					if dropOutput {
						continue
					}
					dst := GetByteBuffer()
					dst, err := v.MarshalMsg(dst[:0])
					if err != nil {
						logger.LogOnceIf(ctx, err, err.Error())
					}
					if !h.sharedResponse {
						h.PutResponse(v)
					}
					select {
					case <-ctx.Done():
						dropOutput = true
					case out <- dst:
					}
				}
			}()
			rErr := handle(ctx, plT, inT, outT)
			close(outT)
			<-outDone
			return rErr
		}, OutCapacity: h.OutCapacity, InCapacity: h.InCapacity, Subroute: strings.Join(subroute, "/"),
	})
}

// TypedStream is a stream with specific types.
type TypedStream[Req, Resp RoundTripper] struct {
	// responses from the remote server.
	// Channel will be closed after error or when remote closes.
	// responses *must* be read to either an error is returned or the channel is closed.
	responses *Stream
	newResp   func() Resp

	// Requests sent to the server.
	// If the handler is defined with 0 incoming capacity this will be nil.
	// Channel *must* be closed to signal the end of the stream.
	// If the request context is canceled, the stream will no longer process requests.
	Requests chan<- Req
}

// Results returns the results from the remote server one by one.
// If any error is returned by the callback, the stream will be canceled.
// If the context is canceled, the stream will be canceled.
func (s *TypedStream[Req, Resp]) Results(next func(resp Resp) error) (err error) {
	return s.responses.Results(func(b []byte) error {
		resp := s.newResp()
		_, err := resp.UnmarshalMsg(b)
		if err != nil {
			return err
		}
		return next(resp)
	})
}

// Streamer creates a stream.
type Streamer interface {
	NewStream(ctx context.Context, h HandlerID, payload []byte) (st *Stream, err error)
}

// Call the remove with the request and
func (h *StreamTypeHandler[Payload, Req, Resp]) Call(ctx context.Context, c Streamer, payload Payload) (st *TypedStream[Req, Resp], err error) {
	var payloadB []byte
	if h.WithPayload {
		var err error
		payloadB, err = payload.MarshalMsg(GetByteBuffer()[:0])
		if err != nil {
			return nil, err
		}
	}
	stream, err := c.NewStream(ctx, h.id, payloadB)
	PutByteBuffer(payloadB)
	if err != nil {
		return nil, err
	}

	// respT := make(chan TypedResponse[Resp])
	var reqT chan Req
	if h.InCapacity > 0 {
		reqT = make(chan Req)
		// Request handler
		go func() {
			defer close(stream.Requests)
			for req := range reqT {
				b, err := req.MarshalMsg(GetByteBuffer()[:0])
				if err != nil {
					logger.LogOnceIf(ctx, err, err.Error())
				}
				h.PutRequest(req)
				stream.Requests <- b
			}
		}()
	} else if stream.Requests != nil {
		close(stream.Requests)
	}

	return &TypedStream[Req, Resp]{responses: stream, newResp: h.NewResponse, Requests: reqT}, nil
}

// NoPayload is a type that can be used for handlers that do not use a payload.
type NoPayload struct{}

// Msgsize returns 0.
func (p NoPayload) Msgsize() int {
	return 0
}

// UnmarshalMsg satisfies the interface, but is a no-op.
func (NoPayload) UnmarshalMsg(bytes []byte) ([]byte, error) {
	return bytes, nil
}

// MarshalMsg satisfies the interface, but is a no-op.
func (NoPayload) MarshalMsg(bytes []byte) ([]byte, error) {
	return bytes, nil
}

// NewNoPayload returns an empty NoPayload struct.
func NewNoPayload() NoPayload {
	return NoPayload{}
}
