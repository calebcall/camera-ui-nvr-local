package main

import (
	"net/url"
	"strings"
)

// backchannelParam is the query parameter camera.ui's SDK appends to a
// source URL when two-way audio is available (sdk/utils.go: "backchannel=
// opus,pcma,pcmu").
const backchannelParam = "backchannel"

// stripBackchannel removes the backchannel request from a stream URL.
//
// A recorder is a one-way capture and never needs a backchannel, but
// CameraDeviceSource.SourceURL() hands back the URL core built for general
// streaming, which asks for one. On a camera's main stream that is harmless
// — it genuinely has a backchannel. On a substream it is not: go2rtc answers
// a backchannel request on a stream that has none with an extra audio track
// whose clock disagrees with the recorded audio, and the mp4 muxer rejects
// the result:
//
//	Application provided invalid, non monotonically increasing dts to muxer
//	in stream 1: 28704 >= 980
//
// which killed every secondary-role recorder about every 36 seconds. Same
// stream recorded with this parameter removed runs clean, with audio intact.
//
// Parameters are rebuilt in their original order rather than through
// url.Values.Encode(), which sorts alphabetically and drops the valueless
// flag form ("?video&audio") that go2rtc relies on. Anything unparseable is
// returned as-is: a slightly odd URL ffmpeg might still accept beats no
// recording at all.
func stripBackchannel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return raw
	}

	parts := strings.Split(u.RawQuery, "&")
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		// Match the exact parameter, in both "backchannel=..." and bare
		// "backchannel" forms — never a name that merely contains it.
		if name, _, _ := strings.Cut(p, "="); name == backchannelParam {
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == len(parts) {
		return raw
	}

	u.RawQuery = strings.Join(kept, "&")
	return u.String()
}
