// Response silence detection for reasonix-telegram.
// When the model returns a silence marker like [SILENT] or NO_REPLY,
// the bridge suppresses the response entirely instead of sending it.
package main

import "strings"

var silenceMarkers = map[string]bool{
	"[SILENT]": true,
	"SILENT":   true,
	"NO_REPLY": true,
	"NO REPLY": true,
}

// isIntentionalSilence checks if the entire response is a silence marker.
// Only returns true when the text is exactly a marker (not embedded in longer text).
// Empty output is not silence (it's an error path).
func isIntentionalSilence(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	// Any text longer than 64 chars can't be a silence marker
	if len(text) > 64 {
		return false
	}
	normalized := strings.ToUpper(strings.Join(strings.Fields(text), " "))
	return silenceMarkers[normalized]
}
