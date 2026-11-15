package model

import (
	"context"
	"sync/atomic"
	"time"
)

// Label says what a model call is for, so a person watching can tell a review from a second look or a fix.
type Label struct {
	Kind  string // review, second look, fix, incident...
	Title string // what it is about, in a few words
	SHA   string
	Path  string
}

type labelKey struct{}

// WithLabel marks the model calls made with ctx.
func WithLabel(ctx context.Context, l Label) context.Context {
	return context.WithValue(ctx, labelKey{}, l)
}

// LabelOf is the label of ctx, or an empty one.
func LabelOf(ctx context.Context) Label {
	l, _ := ctx.Value(labelKey{}).(Label)
	return l
}

// Call is one model call, as an observer sees it: when it starts, about twice a second while the model writes (Reply
// so far, Usage.ReplyTokens counting the pieces written), and once when it ends (Done).
type Call struct {
	Phase      string        // "reading" the prompt until the first piece of the reply, then "writing"
	FirstToken time.Duration // from the start to the first piece: the time spent reading the prompt
	ID         int64
	Label      Label
	Model      string
	System     string
	User       string
	Reply      string
	Err        string
	Start      time.Time
	Done       bool
	Usage      Usage
}

// Observer, when set, is told about every call when it starts and when it ends. It must return quickly.
var Observer func(Call)

var callSeq atomic.Int64

func nextID() int64 { return callSeq.Add(1) }
