package api

// TranscribeRequest carries one recorded audio clip to transcribe. Audio is the
// clip's bytes, base64-encoded in the standard alphabet with padding, and
// MIMEType is the clip's audio media type as the browser recorded it, such as
// audio/webm;codecs=opus.
type TranscribeRequest struct {
	MIMEType string `json:"mime_type"`
	Audio    string `json:"audio"`
}

// TranscribeResponse is the transcribed text of a clip.
type TranscribeResponse struct {
	Text string `json:"text"`
}
