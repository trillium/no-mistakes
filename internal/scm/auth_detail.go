package scm

import "strings"

// AuthFailureDetailMaxBytes bounds the provider output echoed into the step
// log. Provider CLIs mask tokens in their own output, but the excerpt is still
// untrusted third-party text on a user-facing error path, so keep it short.
const AuthFailureDetailMaxBytes = 512

// AuthFailureDetail renders a CLI's own explanation for a failed auth check.
// Falls back to the exec error when the CLI printed nothing (e.g. it could not
// be started at all). Provider CLIs mask tokens in their own output, but the
// excerpt is still untrusted third-party text on a user-facing error path, so
// output is capped at AuthFailureDetailMaxBytes.
func AuthFailureDetail(out []byte, err error) string {
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		return err.Error()
	}
	if len(detail) > AuthFailureDetailMaxBytes {
		detail = detail[:AuthFailureDetailMaxBytes] + " ... (truncated)"
	}
	return strings.Join(strings.Fields(detail), " ")
}
