package ssh_server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"

	"github.com/GoSeoTaxi/ssh_server/anmitsu/go-shlex"
	gossh "golang.org/x/crypto/ssh"
)

const (
	keepAliveRequestType = "keepalive@openssh.com"
)

// Session provides access to information about an SSH session and methods
// to read and write to the SSH channel with an embedded Channel interface from
// crypto/ssh.
//
// When Command() returns an empty slice, the user requested a shell. Otherwise
// the user is performing an exec with those command arguments.
//
// TODO: Signals
type Session interface {
	gossh.Channel

	// User returns the username used when establishing the SSH connection.
	User() string

	// RemoteAddr returns the net.Addr of the client side of the connection.
	RemoteAddr() net.Addr

	// LocalAddr returns the net.Addr of the server side of the connection.
	LocalAddr() net.Addr

	// Environ returns a copy of strings representing the environment set by the
	// user for this session, in the form "key=value".
	Environ() []string

	// Exit sends an exit status and then closes the session.
	Exit(code int) error

	// Command returns a shell parsed slice of arguments that were provided by the
	// user. Shell parsing splits the command string according to POSIX shell rules,
	// which considers quoting not just whitespace.
	Command() []string

	// RawCommand returns the exact command that was provided by the user.
	RawCommand() string

	// Subsystem returns the subsystem requested by the user.
	Subsystem() string

	// PublicKey returns the PublicKey used to authenticate. If a public key was not
	// used it will return nil.
	PublicKey() PublicKey

	// Context returns the connection's context. The returned context is always
	// non-nil and holds the same data as the Context passed into auth
	// handlers and callbacks.
	//
	// The context is canceled when the client's connection closes or I/O
	// operation fails.
	Context() Context

	// Permissions returns a copy of the Permissions object that was available for
	// setup in the auth handlers via the Context.
	Permissions() Permissions

	// Pty returns PTY information, a channel of window size changes, and a boolean
	// of whether or not a PTY was accepted for this session.
	Pty() (Pty, <-chan Window, bool)

	// X11 returns X11 forwarding information and a boolean of whether or not X11
	// forwarding was accepted for this session.
	X11() (X11, bool)

	// Signals registers a channel to receive signals sent from the client. The
	// channel must handle signal sends or it will block the SSH request loop.
	// Registering nil will unregister the channel from signal sends. During the
	// time no channel is registered signals are buffered up to a reasonable amount.
	// If there are buffered signals when a channel is registered, they will be
	// sent in order on the channel immediately after registering.
	Signals(c chan<- Signal)

	// Break regisers a channel to receive notifications of break requests sent
	// from the client. The channel must handle break requests, or it will block
	// the request handling loop. Registering nil will unregister the channel.
	// During the time that no channel is registered, breaks are ignored.
	Break(c chan<- bool)

	// DisablePTYEmulation disables the session's default minimal PTY emulation.
	// If you're setting the pty's termios settings from the Pty request, use
	// this method to avoid corruption.
	// Currently (2022-03-12) the only emulation implemented is NL-to-CRNL translation (`\n`=>`\r\n`).
	// A call of DisablePTYEmulation must precede any call to Write.
	DisablePTYEmulation()
}

// maxSigBufSize is how many signals will be buffered
// when there is no signal channel specified
const maxSigBufSize = 128

func DefaultSessionHandler(srv *Server, conn *gossh.ServerConn, newChan gossh.NewChannel, ctx Context) {
	ch, reqs, err := newChan.Accept()
	if err != nil {
		// TODO: trigger event callback
		return
	}
	sess := &session{
		Channel:           ch,
		conn:              conn,
		handler:           srv.Handler,
		ptyCb:             srv.PtyCallback,
		x11Cb:             srv.X11Callback,
		sessReqCb:         srv.SessionRequestCallback,
		subsystemHandlers: srv.SubsystemHandlers,
		ctx:               ctx,
	}
	sess.handleRequests(ctx, reqs)
}

type session struct {
	sync.Mutex
	gossh.Channel
	conn              *gossh.ServerConn
	handler           Handler
	subsystemHandlers map[string]SubsystemHandler
	handled           bool
	exited            bool
	pty               *Pty
	x11               *X11
	winch             chan Window
	env               []string
	ptyCb             PtyCallback
	x11Cb             X11Callback
	sessReqCb         SessionRequestCallback
	rawCmd            string
	subsystem         string
	ctx               Context
	// sigMu protects sigCh and sigBuf, it is made separate from the
	// session mutex to reduce the risk of deadlocks while we process
	// buffered signals.
	sigMu               sync.Mutex
	sigCh               chan<- Signal
	sigBuf              []Signal
	breakCh             chan<- bool
	disablePtyEmulation bool
}

func (sess *session) DisablePTYEmulation() {
	sess.disablePtyEmulation = true
}

func (sess *session) Write(p []byte) (n int, err error) {
	if sess.pty != nil && !sess.disablePtyEmulation {
		m := len(p)
		// normalize \n to \r\n when pty is accepted.
		// this is a hardcoded shortcut since we don't support terminal modes.
		p = bytes.Replace(p, []byte{'\n'}, []byte{'\r', '\n'}, -1)
		p = bytes.Replace(p, []byte{'\r', '\r', '\n'}, []byte{'\r', '\n'}, -1)
		n, err = sess.Channel.Write(p)
		if n > m {
			n = m
		}
		return
	}
	return sess.Channel.Write(p)
}

func (sess *session) PublicKey() PublicKey {
	sessionkey := sess.ctx.Value(ContextKeyPublicKey)
	if sessionkey == nil {
		return nil
	}
	return sessionkey.(PublicKey)
}

func (sess *session) Permissions() Permissions {
	// use context permissions because its properly
	// wrapped and easier to dereference
	perms := sess.ctx.Value(ContextKeyPermissions).(*Permissions)
	return *perms
}

func (sess *session) Context() Context {
	return sess.ctx
}

func (sess *session) Exit(code int) error {
	sess.Lock()
	defer sess.Unlock()
	if sess.exited {
		return errors.New("Session.Exit called multiple times")
	}
	sess.exited = true

	status := struct{ Status uint32 }{uint32(code)}
	_, err := sess.SendRequest("exit-status", false, gossh.Marshal(&status))
	if err != nil {
		return err
	}
	return sess.Close()
}

func (sess *session) User() string {
	return sess.conn.User()
}

func (sess *session) RemoteAddr() net.Addr {
	return sess.conn.RemoteAddr()
}

func (sess *session) LocalAddr() net.Addr {
	return sess.conn.LocalAddr()
}

func (sess *session) Environ() []string {
	return append([]string(nil), sess.env...)
}

func (sess *session) RawCommand() string {
	return sess.rawCmd
}

func (sess *session) Command() []string {
	cmd, _ := shlex.Split(sess.rawCmd, true)
	return append([]string(nil), cmd...)
}

func (sess *session) Subsystem() string {
	return sess.subsystem
}

func (sess *session) Pty() (Pty, <-chan Window, bool) {
	if sess.pty != nil {
		return *sess.pty, sess.winch, true
	}
	return Pty{}, sess.winch, false
}

func (sess *session) X11() (X11, bool) {
	if sess.x11 != nil {
		return *sess.x11, true
	}
	return X11{}, false
}

func (sess *session) Signals(c chan<- Signal) {
	sess.sigMu.Lock()
	sess.sigCh = c
	if len(sess.sigBuf) == 0 || sess.sigCh == nil {
		sess.sigMu.Unlock()
		return
	}
	// If we have buffered signals, we need to send them whilst
	// holding the signal mutex to avoid race conditions on sigCh
	// and sigBuf. We also guarantee that calling Signals(ch)
	// followed by Signals(nil) will have depleted the sigBuf when
	// the second call returns and that there will be no more
	// signals on ch. This is done in a goroutine so we can return
	// early and allow the caller to set up processing for the
	// channel even after calling Signals(ch).
	go func() {
		// Here we're relying on the mutex being locked in the outer
		// Signals() function, so we simply unlock it when we're done.
		defer sess.sigMu.Unlock()

		for _, sig := range sess.sigBuf {
			sess.sigCh <- sig
		}
		sess.sigBuf = nil
	}()
}

func (sess *session) Break(c chan<- bool) {
	sess.Lock()
	defer sess.Unlock()
	sess.breakCh = c
}

func (sess *session) handleRequests(ctx Context, reqs <-chan *gossh.Request) {
	keepAlive := ctx.KeepAlive()
	defer keepAlive.Close()

	var keepAliveRequestInProgress sync.Mutex
	for {
		select {
		case <-keepAlive.Ticks():
			if keepAlive.TimeIsUp() {
				log.Println("Keep-alive reply not received. Close down the session.")
				_ = sess.Close()
				return
			}

			done := keepAliveRequestInProgress.TryLock()
			if !done {
				continue
			}

			go func() {
				defer keepAliveRequestInProgress.Unlock()

				// Server-initiated keep-alive flow on the client side:
				// client: receive packet: type 98 (SSH_MSG_CHANNEL_REQUEST)
				// client: client_input_channel_req: channel 0 rtype keepalive@openssh.com reply 1
				// client: send packet: type 100 (SSH_MSG_CHANNEL_FAILURE)
				//
				// Apparently, OpenSSH client always replies with 100, but it does not matter
				// as the server considers it as alive (only the response status is ignored).
				_, err := sess.SendRequest(keepAliveRequestType, true, nil)
				keepAlive.ServerRequestedKeepAliveCallback()
				if err != nil && err != io.EOF {
					log.Printf("Sending keep-alive request failed: %v", err)
				} else if err == nil {
					keepAlive.Reset()
				}
			}()
		case req, ok := <-reqs:
			if !ok {
				return
			}

			switch req.Type {
			case "shell", "exec":
				if sess.handled {
					req.Reply(false, nil)
					continue
				}

				var payload = struct{ Value string }{}
				gossh.Unmarshal(req.Payload, &payload)
				sess.rawCmd = payload.Value

				// If there's a session policy callback, we need to confirm before
				// accepting the session.
				if sess.sessReqCb != nil && !sess.sessReqCb(sess, req.Type) {
					sess.rawCmd = ""
					req.Reply(false, nil)
					continue
				}

				sess.handled = true
				req.Reply(true, nil)

				go func() {
					sess.handler(sess)
					sess.Exit(0)
				}()
			case "subsystem":
				if sess.handled {
					req.Reply(false, nil)
					continue
				}

				var payload = struct{ Value string }{}
				gossh.Unmarshal(req.Payload, &payload)
				sess.subsystem = payload.Value

				// If there's a session policy callback, we need to confirm before
				// accepting the session.
				if sess.sessReqCb != nil && !sess.sessReqCb(sess, req.Type) {
					sess.rawCmd = ""
					req.Reply(false, nil)
					continue
				}

				handler := sess.subsystemHandlers[payload.Value]
				if handler == nil {
					handler = sess.subsystemHandlers["default"]
				}
				if handler == nil {
					req.Reply(false, nil)
					continue
				}

				sess.handled = true
				req.Reply(true, nil)

				go func() {
					handler(sess)
					sess.Exit(0)
				}()
			case "env":
				if sess.handled {
					req.Reply(false, nil)
					continue
				}
				var kv struct{ Key, Value string }
				gossh.Unmarshal(req.Payload, &kv)
				sess.env = append(sess.env, fmt.Sprintf("%s=%s", kv.Key, kv.Value))
				req.Reply(true, nil)
			case "signal":
				var payload struct{ Signal string }
				gossh.Unmarshal(req.Payload, &payload)
				sess.sigMu.Lock()
				if sess.sigCh != nil {
					sess.sigCh <- Signal(payload.Signal)
				} else {
					if len(sess.sigBuf) < maxSigBufSize {
						sess.sigBuf = append(sess.sigBuf, Signal(payload.Signal))
					}
				}
				sess.sigMu.Unlock()
			case "pty-req":
				if sess.handled || sess.pty != nil {
					req.Reply(false, nil)
					continue
				}
				ptyReq, ok := parsePtyRequest(req.Payload)
				if !ok {
					req.Reply(false, nil)
					continue
				}
				if sess.ptyCb != nil {
					ok := sess.ptyCb(sess.ctx, ptyReq)
					if !ok {
						req.Reply(false, nil)
						continue
					}
				}
				sess.pty = &ptyReq
				sess.winch = make(chan Window, 1)
				sess.winch <- ptyReq.Window
				defer func() {
					// when reqs is closed
					close(sess.winch)
				}()
				req.Reply(ok, nil)
			case "x11-req":
				if sess.handled || sess.x11 != nil {
					req.Reply(false, nil)
					continue
				}
				x11Req, ok := parseX11Request(req.Payload)
				if !ok {
					req.Reply(false, nil)
					continue
				}
				sess.x11 = &x11Req
				if sess.x11Cb != nil {
					ok := sess.x11Cb(sess.ctx, x11Req)
					req.Reply(ok, nil)
					continue
				}
				req.Reply(false, nil)
			case "window-change":
				if sess.pty == nil {
					req.Reply(false, nil)
					continue
				}
				win, _, ok := parseWindow(req.Payload)
				if ok {
					sess.pty.Window = win
					sess.winch <- win
				}
				req.Reply(ok, nil)
			case agentRequestType:
				// TODO: option/callback to allow agent forwarding
				SetAgentRequested(sess.ctx)
				req.Reply(true, nil)
			case keepAliveRequestType:
				if req.WantReply {
					req.Reply(true, nil)
				}
			case "break":
				ok := false
				sess.Lock()
				if sess.breakCh != nil {
					sess.breakCh <- true
					ok = true
				}
				req.Reply(ok, nil)
				sess.Unlock()
			default:
				// TODO: debug log
				req.Reply(false, nil)
			}
		}

	}
}

// KeepAliveRequestHandler replies to periodic client keep-alive requests:
// client: send packet: type 80 (SSH_MSG_GLOBAL_REQUEST)
// client: receive packet: type 82 (SSH_MSG_REQUEST_SUCCESS)
func KeepAliveRequestHandler(ctx Context, srv *Server, req *gossh.Request) (ok bool, payload []byte) {
	keepAlive := ctx.KeepAlive()
	if keepAlive != nil {
		ctx.KeepAlive().RequestHandlerCallback()
	}
	return false, nil
}
