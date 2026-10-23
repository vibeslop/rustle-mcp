package rustle

import (
	"errors"
	"fmt"
)

// Error is a safe, actionable error. Its message, code and recovery reach the model verbatim.
type Error struct {
	Message   string
	Code      string
	Recovery  string
	Retryable bool
	// RetryAfter is in seconds; 0 leaves it out.
	RetryAfter int
}

func (e *Error) Error() string { return e.Message }

// Fail is an Error with a code and the recovery an agent should try.
func Fail(message, code, recovery string) *Error {
	if code == "" {
		code = "REQUEST_FAILED"
	}
	if recovery == "" {
		recovery = "Check the source and arguments, then retry."
	}
	return &Error{Message: message, Code: code, Recovery: recovery}
}

// Retry marks the error retryable, after `after` seconds when that is above 0.
func (e *Error) Retry(after int) *Error {
	e.Retryable = true
	e.RetryAfter = after
	return e
}

// Fields is the error as the model reads it.
func (e *Error) Fields() *Obj {
	out := NewObj().Set("error", e.Message).Set("code", e.Code).Set("retryable", e.Retryable).Set("recovery", e.Recovery)
	if e.RetryAfter > 0 {
		out.Set("retry_after_seconds", e.RetryAfter)
	}
	return out
}

// AsError finds the Error in err's chain.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// HasCode reports whether err is an Error with one of these codes.
func HasCode(err error, codes ...string) bool {
	e, ok := AsError(err)
	if !ok {
		return false
	}
	for _, code := range codes {
		if e.Code == code {
			return true
		}
	}
	return false
}

func Invalid(message string) *Error {
	return Fail(message, "INVALID_ARGUMENTS", "Fix the named argument and call again.")
}

func NoVideoTrack(title string) *Error {
	if title == "" {
		title = "This source"
	}
	return Fail(title+" is audio only, so there are no frames to show.", "NO_VIDEO_TRACK",
		"Call watch_video with listen for its sound, or with frames=0 for its speech.")
}

func NoAudioTrack(title string) *Error {
	if title == "" {
		title = "This source"
	}
	return Fail(title+" has no audio track.", "NO_AUDIO_TRACK", "Call watch_video without listen for what is shown.")
}

func Cancelled() *Error {
	return Fail("The call was cancelled.", "CANCELLED", "Call again if it is still needed.")
}

func NetworkError() *Error {
	return Fail("Could not reach the server.", "NETWORK_UNAVAILABLE", "Check network access, wait briefly, then retry.").Retry(0)
}

// HTTPError maps an HTTP status from a media or page host to an error an agent can act on.
func HTTPError(status int, what string) *Error {
	if what == "" {
		what = "The server"
	}
	switch {
	case status == 429:
		return Fail(what+" is rate limiting this connection (HTTP 429).", "RATE_LIMITED", "Wait a few minutes before retrying.").Retry(120)
	case status == 401 || status == 403:
		return Fail(fmt.Sprintf("%s refused access (HTTP %d).", what, status), "ACCESS_DENIED",
			"Use a public source, a direct media URL or a local file.")
	case status == 404 || status == 410:
		return Fail(fmt.Sprintf("%s reports the media as missing (HTTP %d).", what, status), "SOURCE_NOT_FOUND",
			"Check the link or choose another source.")
	}
	e := Fail(fmt.Sprintf("%s returned HTTP %d.", what, status), "UPSTREAM_UNAVAILABLE", "Wait briefly, then retry.")
	e.Retryable = status >= 500 || status == 408
	return e
}
