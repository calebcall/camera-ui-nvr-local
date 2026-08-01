package store

import (
	"encoding/base64"
	"encoding/json"
)

// b64 is how encoding/json renders a []byte, which is the form a stripped
// thumbnail would leave behind in raw if stripping failed.
func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// marshalEventForTest reproduces how the previous version wrote raw: the whole
// event, thumbnails included.
func marshalEventForTest(ev DetectionEvent) (string, error) {
	b, err := json.Marshal(ev)
	return string(b), err
}
