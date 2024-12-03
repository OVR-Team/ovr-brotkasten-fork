package webrtc

import (
	"encoding/json"
	"net/http"
)

func GetMediaAssigner(r *http.Request) (*MediaAssigner, error) {
	var mediaAssigner *MediaAssigner
	err := json.Unmarshal([]byte(r.Header.Get("MediaAssigner")), &mediaAssigner)
	return mediaAssigner, err
}
