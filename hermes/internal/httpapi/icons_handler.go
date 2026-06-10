package httpapi

import (
	"net/http"
)

// IconsDir is the filesystem path to the deploy/icons directory.
// Set via NewServer options before ListenAndServe.
var IconsDir string

// iconsHandler returns an http.Handler that serves icon files from the IconsDir.
func iconsHandler() http.Handler {
	if IconsDir == "" {
		// Return a handler that always returns 404 if icons not configured
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})
	}
	return http.StripPrefix("/icons/", http.FileServer(http.Dir(IconsDir)))
}
