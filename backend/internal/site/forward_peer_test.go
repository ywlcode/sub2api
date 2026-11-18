package site

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

const forwardCompleted = "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"

type forwardPeerPlan struct {
	headersEarly bool
	finishDelay  time.Duration
	heartbeat    time.Duration
	reset        bool
	goAway       bool
	waitUpload   bool
}

type forwardPeer struct {
	ackDelay time.Duration // negative means a silent connection blackhole
	plan     func(int) forwardPeerPlan
	requests atomic.Int32
	resets   atomic.Int32
}

type forwardPeerAction struct {
	kind   string
	stream uint32
	ping   [8]byte
}

func serveForwardPingPeer(conn net.Conn, ackDelay, finishDelay time.Duration, headersEarly bool) error {
	peer := &forwardPeer{ackDelay: ackDelay, plan: func(int) forwardPeerPlan {
		return forwardPeerPlan{headersEarly: headersEarly, finishDelay: finishDelay}
	}}
	return peer.serve(conn)
}

// An actual HTTP/2 peer, with deterministic application and frame failures. All
// writes run on one goroutine; virtual time never relies on CI wall-clock sleeps.
func (p *forwardPeer) serve(conn net.Conn) error {
	framer := http2.NewFramer(conn, conn)
	type readResult struct {
		frame   http2.Frame
		err     error
		dataLen int
	}
	frames := make(chan readResult, 16)
	actions := make(chan forwardPeerAction, 16)
	done := make(chan struct{})
	defer close(done)
	var timers []*time.Timer
	defer func() {
		for _, timer := range timers {
			timer.Stop()
		}
	}()
	schedule := func(delay time.Duration, action forwardPeerAction) {
		timers = append(timers, time.AfterFunc(delay, func() {
			select {
			case actions <- action:
			case <-done:
			}
		}))
	}
	go func() {
		preface := make([]byte, len(http2.ClientPreface))
		_, err := io.ReadFull(conn, preface)
		if err == nil && string(preface) != http2.ClientPreface {
			err = errors.New("invalid HTTP/2 preface")
		}
		for {
			var frame http2.Frame
			dataLen := 0
			if err == nil {
				frame, err = framer.ReadFrame()
				if data, ok := frame.(*http2.DataFrame); ok {
					dataLen = len(data.Data())
				}
			}
			select {
			case frames <- readResult{frame, err, dataLen}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	if err := framer.WriteSettings(); err != nil {
		return err
	}
	sendHeaders := func(id uint32) error {
		var b bytes.Buffer
		encoder := hpack.NewEncoder(&b)
		for _, field := range []hpack.HeaderField{{Name: ":status", Value: "200"}, {Name: "content-type", Value: "text/event-stream"}} {
			if err := encoder.WriteField(field); err != nil {
				return err
			}
		}
		return framer.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: b.Bytes(), EndHeaders: true})
	}
	active := make(map[uint32]forwardPeerPlan)
	begin := func(id uint32, plan forwardPeerPlan) error {
		if plan.headersEarly {
			if err := sendHeaders(id); err != nil {
				return err
			}
			if err := framer.WriteData(id, false, []byte("data: first\n\n")); err != nil {
				return err
			}
		}
		if plan.reset {
			return framer.WriteRSTStream(id, http2.ErrCodeInternal)
		}
		if !plan.waitUpload {
			schedule(plan.finishDelay, forwardPeerAction{kind: "finish", stream: id})
		}
		if plan.heartbeat > 0 {
			schedule(plan.heartbeat, forwardPeerAction{kind: "heartbeat", stream: id})
		}
		return nil
	}
	for {
		select {
		case result := <-frames:
			if result.err != nil {
				return result.err
			}
			switch f := result.frame.(type) {
			case *http2.SettingsFrame:
				if !f.IsAck() {
					if err := framer.WriteSettingsAck(); err != nil {
						return err
					}
				}
			case *http2.HeadersFrame:
				plan := p.plan(int(p.requests.Add(1)))
				active[f.StreamID] = plan
				if err := begin(f.StreamID, plan); err != nil {
					return err
				}
				if f.StreamEnded() && plan.waitUpload {
					schedule(plan.finishDelay, forwardPeerAction{kind: "finish", stream: f.StreamID})
				}
			case *http2.DataFrame:
				// Return flow-control credit, allowing large upload test bodies.
				if n := result.dataLen; n > 0 {
					if err := framer.WriteWindowUpdate(0, uint32(n)); err != nil {
						return err
					}
					if err := framer.WriteWindowUpdate(f.StreamID, uint32(n)); err != nil {
						return err
					}
				}
				if plan, ok := active[f.StreamID]; ok && plan.waitUpload && f.StreamEnded() {
					schedule(plan.finishDelay, forwardPeerAction{kind: "finish", stream: f.StreamID})
				}
			case *http2.RSTStreamFrame:
				p.resets.Add(1)
				delete(active, f.StreamID)
			case *http2.PingFrame:
				if !f.IsAck() && p.ackDelay >= 0 {
					schedule(p.ackDelay, forwardPeerAction{kind: "ping", ping: f.Data})
				}
			}
		case action := <-actions:
			if action.kind == "ping" {
				if err := framer.WritePing(true, action.ping); err != nil {
					return err
				}
				continue
			}
			plan, ok := active[action.stream]
			if !ok {
				continue
			}
			switch action.kind {
			case "heartbeat":
				if err := framer.WriteData(action.stream, false, []byte(": keepalive\n\n")); err != nil {
					return err
				}
				schedule(plan.heartbeat, action)
			case "finish":
				if !plan.headersEarly {
					if err := sendHeaders(action.stream); err != nil {
						return err
					}
				}
				if err := framer.WriteData(action.stream, true, []byte(forwardCompleted)); err != nil {
					return err
				}
				delete(active, action.stream)
				if plan.goAway {
					if err := framer.WriteGoAway(action.stream, http2.ErrCodeNo, nil); err != nil {
						return err
					}
				}
			}
		}
	}
}
