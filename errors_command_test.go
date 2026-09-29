package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestErrorsRefusesExtraArguments(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runErrors([]string{"--last", "3", "extra"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), `unexpected argument "extra"`) || out.Len() != 0 {
		t.Fatalf("stderr = %q, stdout = %q", errOut.String(), out.String())
	}
	if code := runErrors([]string{"--last", "0"}, &out, &errOut); code != 2 {
		t.Fatalf("--last 0 exit = %d, want 2", code)
	}
}
