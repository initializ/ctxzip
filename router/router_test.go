package router

import (
	"testing"

	"github.com/initializ/ctxzip/detect"
)

func TestRouter_For(t *testing.T) {
	r := New()
	cases := map[detect.ContentType]string{
		detect.JSONArray:     "json_crusher",
		detect.BuildLog:      "log_crusher",
		detect.YAMLLike:      "yaml_crusher",
		detect.PlainText:     "text_extractive",
		detect.SearchResults: "text_extractive",
		detect.GitDiff:       "text_extractive",
		detect.SourceCode:    "text_extractive",
	}
	for ct, want := range cases {
		if got := r.For(ct).Name(); got != want {
			t.Errorf("For(%v) = %s, want %s", ct, got, want)
		}
	}
}

func TestRouter_UnknownDefaultsToText(t *testing.T) {
	r := New()
	if got := r.For(detect.ContentType("???")).Name(); got != "text_extractive" {
		t.Errorf("unknown type routed to %s, want text_extractive", got)
	}
}
