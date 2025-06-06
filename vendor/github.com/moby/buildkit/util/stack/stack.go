package stack

import (
	"fmt"
	io "io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/containerd/typeurl/v2"
	"github.com/pkg/errors"
)

var helpers map[string]struct{}
var helpersMu sync.RWMutex

func init() {
	typeurl.Register((*Stack)(nil), "github.com/moby/buildkit", "stack.Stack+json")

	helpers = map[string]struct{}{}
}

var version string
var revision string

func SetVersionInfo(v, r string) {
	version = v
	revision = r
}

func Helper() {
	var pc [1]uintptr
	n := runtime.Callers(2, pc[:])
	if n == 0 {
		return
	}
	frames := runtime.CallersFrames(pc[:n])
	frame, _ := frames.Next()
	helpersMu.Lock()
	helpers[frame.Function] = struct{}{}
	helpersMu.Unlock()
}

func Traces(err error) []*Stack {
	return compressStacks(traces(err))
}

func traces(err error) []*Stack {
	var st []*Stack

	switch e := err.(type) { //nolint:errorlint
	case interface{ Unwrap() error }:
		st = Traces(e.Unwrap())
	case interface{ Unwrap() []error }:
		for _, ue := range e.Unwrap() {
			st = Traces(ue)
			// Only take first stack
			if len(st) > 0 {
				break
			}
		}
	}

	switch ste := err.(type) { //nolint:errorlint
	case interface{ StackTrace() errors.StackTrace }:
		st = append(st, convertStack(ste.StackTrace()))
	case interface{ StackTrace() *Stack }:
		st = append(st, ste.StackTrace())
	}

	return st
}

func Enable(err error) error {
	if err == nil {
		return nil
	}
	Helper()
	if !hasLocalStackTrace(err) {
		return errors.WithStack(err)
	}
	return err
}

func Wrap(err error, s *Stack) error {
	return &withStackError{stack: s, error: err}
}

func hasLocalStackTrace(err error) bool {
	wrapped, ok := err.(interface {
		Unwrap() error
	})
	if ok && hasLocalStackTrace(wrapped.Unwrap()) {
		return true
	}

	_, ok = err.(interface {
		StackTrace() errors.StackTrace
	})
	return ok
}

func Formatter(err error) fmt.Formatter {
	return &formatter{err}
}

type formatter struct {
	error
}

func (w *formatter) Format(s fmt.State, verb rune) {
	if w.error == nil {
		fmt.Fprintf(s, "%v", w.error)
		return
	}
	switch verb {
	case 'v':
		if s.Flag('+') {
			fmt.Fprintf(s, "%s\n", w.Error())
			for _, stack := range Traces(w.error) {
				fmt.Fprintf(s, "%d %s %s\n", stack.Pid, stack.Version, strings.Join(stack.Cmdline, " "))
				for _, f := range stack.Frames {
					fmt.Fprintf(s, "%s\n\t%s:%d\n", f.Name, f.File, f.Line)
				}
				fmt.Fprintln(s)
			}
			return
		}
		fallthrough
	case 's':
		io.WriteString(s, w.Error())
	case 'q':
		fmt.Fprintf(s, "%q", w.Error())
	}
}

func convertStack(s errors.StackTrace) *Stack {
	var out Stack
	helpersMu.RLock()
	defer helpersMu.RUnlock()
	for _, f := range s {
		dt, err := f.MarshalText()
		if err != nil {
			continue
		}
		p := strings.SplitN(string(dt), " ", 2)
		if len(p) != 2 {
			continue
		}
		if _, ok := helpers[p[0]]; ok {
			continue
		}
		idx := strings.LastIndexByte(p[1], ':')
		if idx == -1 {
			continue
		}
		line, err := strconv.ParseInt(p[1][idx+1:], 10, 32)
		if err != nil {
			continue
		}
		out.Frames = append(out.Frames, &Frame{
			Name: p[0],
			File: p[1][:idx],
			Line: int32(line),
		})
	}
	out.Cmdline = os.Args
	out.Pid = int32(os.Getpid())
	out.Version = version
	out.Revision = revision
	return &out
}

type withStackError struct {
	stack *Stack
	error
}

func (e *withStackError) Unwrap() error {
	return e.error
}

func (e *withStackError) StackTrace() *Stack {
	return e.stack
}
