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
	json *crush.JSONCrusher
	log  *crush.LogCrusher
	text *crush.TextCrusher
}

// New builds a Router with the default compressors.
func New() *Router {
	return &Router{
		json: crush.NewJSONCrusher(),
		log:  crush.NewLogCrusher(),
		text: crush.NewTextCrusher(),
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
	case detect.PlainText, detect.SearchResults:
		// Prose and search output both compress acceptably with extractive
		// sentence/line selection until dedicated crushers land.
		return r.text
	case detect.GitDiff, detect.SourceCode:
		// TODO: dedicated diff/AST crushers. Route to extractive text for now —
		// it only drops near-duplicate lines without a query, so it is safe.
		return r.text
	default:
		return r.text
	}
}
