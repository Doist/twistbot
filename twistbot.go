// Package twistbot provides abstractions to build http endpoints working as
// Twist bot integrations.
package twistbot

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"sync"
	"time"

	"github.com/artyom/httpflags"
	"golang.org/x/sync/errgroup"
)

const (
	OriginComment = "comment" // message comes from comment of a thread
	OriginMessage = "message" // message comes from direct conversation
	OriginThread  = "thread"  // message comes from thread's opening post
)

// DefaultUsage is a text sent by Handler if none of its rules matched message
// and Handler's Usage is not set.
const DefaultUsage = "Sorry, I'm a bot, I don't know what you mean."

// Message presents data received from Twist bot integration.
type Message struct {
	// Text holds text of the message addressed to bot integration
	Text string `flag:"content"`

	// Origin can be one of comment | message | thread, see Origin*
	// constants
	Origin string `flag:"event_type"`

	// AsyncReplyURL is where asynchronous (delayed) reply should be posted
	// to, if needed.
	AsyncReplyURL string `flag:"url_callback"`

	// TTL is a unix timestamp after which replies posted to AsyncReplyURL
	// would be rejected.
	TTL int64 `flag:"url_ttl"`

	// Workspace id
	Workspace uint64 `flag:"workspace_id"`

	// UserID is the id of message author
	UserID uint64 `flag:"user_id"`

	// UserName is the name of message author
	UserName string `flag:"user_name"`

	// Token is used to verify authenticity of message — its unique value is
	// assigned to each integration.
	Token string `flag:"verify_token"`
}

// Context returns context derived from parent which is canceled either when
// parent expires or reply deadline for this message is reached.
func (m Message) Context(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithDeadline(parent, time.Unix(m.TTL, 0))
}

// ReplyDeadline returns message reply deadline. Replies made after this time
// would be rejected.
func (m Message) ReplyDeadline() time.Time { return time.Unix(m.TTL, 0) }

// Reply sends asynchronous reply to a given Message. It automatically takes
// into account reply deadline. If client is nil, http.DefaultClient is used.
//
// Attachments are zero or more opaque attachment object, see Uploader
// documentation on how to upload file.
func (msg Message) Reply(ctx context.Context, client *http.Client, text string, attachments ...json.RawMessage) error {
	if text == "" {
		return errors.New("reply cannot be empty")
	}
	payload := struct {
		Text        string            `json:"content"`
		Attachments []json.RawMessage `json:"attachments,omitempty"`
	}{Text: text, Attachments: attachments}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, msg.AsyncReplyURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	ctx, cancel := msg.Context(ctx)
	defer cancel()
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code on reply post: %q", resp.Status)
	}
	return nil
}

// Handler accepts webhook call from Twist bot integration, decodes request into
// a Message, verifies its authenticity by comparing it with Token, then tries
// to process message by rules in order they were added by AddRule method.
type Handler struct {
	Token string // token used to verify incoming messages
	// text to reply with if no rules matched; if empty, DefaultUsage is
	// used
	Usage string
	rules []Rule
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	var msg Message
	if err := httpflags.Parse(&msg, r); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	if h.Token == "" || msg.Token != h.Token {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	ww := &trackedResponseWriter{ResponseWriter: w}
	var i int
	var fn Rule
	for i, fn = range h.rules {
		if ww.dirty() {
			panic(fmt.Sprintf("twistbot: http.ResponseWriter was already used by rule #%d", i-1))
		}
		switch err := fn(r.Context(), ww, msg); err {
		case nil:
			return
		case SkipRule:
			continue
		default:
			logFunc(r)("rule #%d: %v", i, err)
			if !ww.dirty() {
				http.Error(ww, http.StatusText(http.StatusInternalServerError),
					http.StatusInternalServerError)
			}
			return
		}
	}
	if ww.dirty() {
		panic(fmt.Sprintf("twistbot: http.ResponseWriter was already used by rule #%d", i))
	}
	ww.Header().Set("Content-Type", "text/plain; charset=utf-8")
	switch h.Usage {
	case "":
		fmt.Fprintln(ww, DefaultUsage)
	default:
		fmt.Fprintln(ww, h.Usage)
	}
}

// AddRule plugs rule into Handler. Handler probes incoming Message against its
// rules in order they were added.
func (h *Handler) AddRule(rule Rule) {
	if rule == nil {
		panic("twistbot: Rule cannot be nil")
	}
	h.rules = append(h.rules, rule)
}

// Rule is a function processing incoming message and replying on it. If rule
// doesn't know how to process message, it must return SkipRule without touching
// ResponseWriter. If rule successfully processed message, it must reply with
// nil to stop further processing; if rule replies with error, this error is
// logged to http.Server.ErrorLog and handler replies with 500 status code when
// appropriate (if ResponseWriter was not used yet).
//
// Context passed is the one from incoming http.Request; if rule needs to do
// some background processing and reply asynchronously, it should create new
// context from message.
//
// If function decides to reply right away, reply should be written plaintext to
// provided ResponseWriter. It's valid to not reply anything and just return
// nil. Function may decide to do a follow-up reply as a result of some
// background asynchronous operation. To do this, use Message.Reply method that
// posts reply to Message.AsyncReplyURL.
type Rule func(context.Context, http.ResponseWriter, Message) error

// SkipDir is used as a return value from Rules to indicate that rule must be
// skipped.
var SkipRule = errors.New("skip rule")

// Uploader uploads files to Twist as attachments.
type Uploader struct {
	// Token to access Twist API. It is different from the one used to
	// verify messages.
	Token string
	// http client to use, if nil, http.DefaultClient is used
	Client *http.Client
}

// Upload uploads data read from r under given name and returns opaque json
// object describing uploaded object. It can then be used to describe attachment
// when replying to message.
func (upl *Uploader) Upload(ctx context.Context, r io.Reader, name string) (uploadInfo json.RawMessage, err error) {
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	mw := multipart.NewWriter(pw)
	var group errgroup.Group
	group.Go(func() error {
		defer pw.Close()
		if err := mw.WriteField("attachment_id", randomString()); err != nil {
			return err
		}
		fw, err := mw.CreateFormFile("file_name", name)
		if err != nil {
			return err
		}
		if _, err := io.Copy(fw, r); err != nil {
			return err
		}
		return mw.Close()
	})
	group.Go(func() error {
		defer pr.Close()
		req, err := http.NewRequest(http.MethodPost, "https://api.twistapp.com/api/v3/attachments/upload", pr)
		if err != nil {
			return err
		}
		req = req.WithContext(ctx)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+upl.Token)
		client := upl.Client
		if client == nil {
			client = http.DefaultClient
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status code on upload: %q", resp.Status)
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<18)).Decode(&uploadInfo)
	})
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return uploadInfo, nil
}

func randomString() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// trackedResponseWriter wraps http.ResponseWriter and tracks whether
// WriteHeader or Write were called
type trackedResponseWriter struct {
	http.ResponseWriter
	once sync.Once
	used bool // true if WriteHeader or Write was already called
}

func (tr *trackedResponseWriter) WriteHeader(statusCode int) {
	tr.mark()
	tr.ResponseWriter.WriteHeader(statusCode)
}

func (tr *trackedResponseWriter) Write(b []byte) (int, error) {
	tr.mark()
	return tr.ResponseWriter.Write(b)
}

func (tr *trackedResponseWriter) mark() { tr.once.Do(func() { tr.used = true }) }

// dirty returns true if WriteHeader or Write methods were called or if any
// headers were set
func (tr *trackedResponseWriter) dirty() bool { return tr.used || len(tr.Header()) != 0 }

// logFunc returns Printf-like function from http.Request which logs to
// associated http.Server.ErrorLog, if it's non-nil, or is noop otherwise.
func logFunc(r *http.Request) func(format string, v ...interface{}) {
	srv, ok := r.Context().Value(http.ServerContextKey).(*http.Server)
	if ok && srv.ErrorLog != nil {
		return srv.ErrorLog.Printf
	}
	return func(string, ...interface{}) {}
}
