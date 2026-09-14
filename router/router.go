// Package router maps a detected content type to the compressor that handles
// it. It owns one instance of each compressor, all sharing the caller's
// ccr.Store.
package router

import (
	"github.com/initializ/ctxzip/crush"
	"github.com/initializ/ctxzip/detect"
)

// Router selects a compressor for a content type.
type Router struct {
	json   *crush.JSONCrusher
	log    *crush.LogCrusher
	text   *crush.TextCrusher
	yaml   *crush.YAMLCrusher
	diff   *crush.DiffCrusher
	search *crush.SearchCrusher
	code   *crush.CodeCrusher
}

// New builds a Router with the default compressors.
func New() *Router {
	return &Router{
		json:   crush.NewJSONCrusher(),
		log:    crush.NewLogCrusher(),
		text:   crush.NewTextCrusher(),
		yaml:   crush.NewYAMLCrusher(),
		diff:   crush.NewDiffCrusher(),
		search: crush.NewSearchCrusher(),
		code:   crush.NewCodeCrusher(),
	}
}

// For returns the compressor for ct. Content types without a dedicated
// structure-aware strategy fall through to the extractive text crusher.
func (r *Router) For(ct detect.ContentType) crush.Compressor {
	switch ct {
	case detect.JSONArray:
		return r.json
	case detect.BuildLog:
		return r.log
	case detect.YAMLLike:
		return r.yaml
	case detect.SearchResults:
		return r.search
	case detect.PlainText:
		return r.text
	case detect.GitDiff:
		return r.diff
	case detect.SourceCode:
		return r.code
	default:
		return r.text
	}
}
