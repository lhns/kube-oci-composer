package build

import (
	"strings"
	"testing"
)

// TestCheckPinnedBasesAccepts: stage aliases and `scratch` are not registry references.
func TestCheckPinnedBasesAccepts(t *testing.T) {
	const digest = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	cases := map[string]string{
		"pinned":  "FROM golang:1.26" + digest + "\nRUN go build ./...\n",
		"scratch": "FROM scratch\nCOPY x /x\n",
		"multi-stage with alias": "FROM golang:1.26" + digest + " AS builder\n" +
			"RUN go build\n" +
			"FROM gcr.io/distroless/static" + digest + "\n" +
			"COPY --from=builder /app /app\n",
		"stage reference": "FROM golang:1.26" + digest + " AS build\n" +
			"FROM build\n",
		"lowercase from":  "from busybox" + digest + "\n",
		"comments blank":  "# a comment\n\n   \nFROM busybox" + digest + "\n",
		"continuation":    "FROM \\\n  busybox" + digest + "\n",
		"trailing fields": "FROM busybox" + digest + " AS final\n",
	}

	for name, df := range cases {
		t.Run(name, func(t *testing.T) {
			if err := CheckPinnedBases(strings.NewReader(df)); err != nil {
				t.Errorf("rejected: %v", err)
			}
		})
	}
}

// TestCheckPinnedBasesRefuses: an unchanged spec must not build on a different base.
func TestCheckPinnedBasesRefuses(t *testing.T) {
	const digest = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	cases := map[string]string{
		"bare tag":      "FROM golang:1.26\n",
		"no tag":        "FROM golang\n",
		"arg base":      "ARG BASE=golang:1.26\nFROM $BASE\n",
		"arg braced":    "FROM ${BASE}\n",
		"second stage":  "FROM golang:1.26" + digest + " AS b\nFROM alpine:3\n",
		"alias not ref": "FROM golang:1.26" + digest + " AS builder\nFROM buildr\n",
	}

	for name, df := range cases {
		t.Run(name, func(t *testing.T) {
			err := CheckPinnedBases(strings.NewReader(df))
			if err == nil {
				t.Fatal("accepted an unpinned base")
			}
			if !strings.Contains(err.Error(), "pinned by digest") {
				t.Errorf("error %q does not explain the rule", err)
			}
		})
	}
}

// TestCheckPinnedBasesReportsEveryOffender, so one edit fixes them all.
func TestCheckPinnedBasesReportsEveryOffender(t *testing.T) {
	err := CheckPinnedBases(strings.NewReader("FROM a:1\nFROM b:2\nFROM c:3\n"))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{`"a:1"`, `"b:2"`, `"c:3"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

// TestCheckPinnedBasesIgnoresNonFrom: "FROM" inside another instruction is not an instruction.
func TestCheckPinnedBasesIgnoresNonFrom(t *testing.T) {
	const digest = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	df := "FROM busybox" + digest + "\n" +
		"RUN echo FROM alpine:3\n" +
		"COPY --from=0 /a /b\n" +
		"LABEL description=\"builds FROM source\"\n"

	if err := CheckPinnedBases(strings.NewReader(df)); err != nil {
		t.Errorf("a non-FROM line was treated as an instruction: %v", err)
	}
}
