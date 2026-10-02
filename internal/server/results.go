package server

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
)

// resultsDirEnv names the env var holding the benchmark results directory. It
// is an env var rather than a config option because a config option needs a
// docs/kb guide, and the directory is a bind-mounted sibling repo
// (/opt/ai/artisanal-inference/docs/results), not llama-swap state.
const resultsDirEnv = "LLAMA_SWAP_RESULTS_DIR"

// resultsFiles is the allowlist of servable names, with explicit Content-Types
// because Go's mime table has no .jsonl. A lookup in this map (never a path
// join of user input) is what keeps ".." and subpaths out.
var resultsFiles = map[string]string{
	"catalog.json":       "application/json",
	"schema.json":        "application/json",
	"measurements.jsonl": "application/x-ndjson",
}

// handleResults serves one allowlisted results file, read from disk per
// request so edits show up immediately. The quoted ETag lets ServeContent
// answer If-None-Match with 304.
func (s *Server) handleResults(w http.ResponseWriter, r *http.Request) {
	if s.resultsDir == "" {
		writeRGError(w, http.StatusNotFound, "results_not_configured", resultsDirEnv+" is not set")
		return
	}
	name := r.PathValue("name")
	contentType, ok := resultsFiles[name]
	if !ok {
		writeRGError(w, http.StatusNotFound, "results_unknown_file", fmt.Sprintf("%q is not a results file", name))
		return
	}

	// OpenInRoot (not Open of a joined path) because a symlink planted in the
	// results dir must not be able to point the server at a file outside it.
	f, err := os.OpenInRoot(s.resultsDir, name)
	if err == nil {
		defer f.Close()
	}
	var info fs.FileInfo
	if err == nil {
		info, err = f.Stat()
	}
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file (mode %s)", name, info.Mode())
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeRGError(w, http.StatusNotFound, "results_file_missing", name+" does not exist in "+resultsDirEnv)
			return
		}
		// The real error goes to the log only, because it carries host paths
		// that the tailnet-readable API has no reason to expose.
		s.logs.ProxyLogs.Warnf("results: reading %s: %v", name, err)
		writeRGError(w, http.StatusInternalServerError, "results_read_failed", "could not read "+name)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, info.Size(), info.ModTime().UnixNano()))
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, name, info.ModTime(), f)
}
