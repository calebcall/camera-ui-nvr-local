package main

import "testing"

func TestStripBackchannel_RemovesTheParameter(t *testing.T) {
	in := "rtsp://127.0.0.1:2001/cui_pool_sub?video&audio&backchannel=opus,pcma,pcmu&timeout=15"
	want := "rtsp://127.0.0.1:2001/cui_pool_sub?video&audio&timeout=15"

	if got := stripBackchannel(in); got != want {
		t.Fatalf("\n got: %s\nwant: %s", got, want)
	}
}

// Every other parameter, and their order, must survive untouched — the
// recorder depends on video/audio/timeout being exactly what core chose.
func TestStripBackchannel_PreservesEverythingElse(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"backchannel first",
			"rtsp://h/s?backchannel=opus,pcma,pcmu&video&audio&timeout=15",
			"rtsp://h/s?video&audio&timeout=15",
		},
		{
			"backchannel last",
			"rtsp://h/s?video&audio&timeout=15&backchannel=opus",
			"rtsp://h/s?video&audio&timeout=15",
		},
		{
			"only parameter",
			"rtsp://h/s?backchannel=opus",
			"rtsp://h/s",
		},
		{
			"valueless flag form",
			"rtsp://h/s?video&backchannel&timeout=15",
			"rtsp://h/s?video&timeout=15",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripBackchannel(c.in); got != c.want {
				t.Fatalf("\n got: %s\nwant: %s", got, c.want)
			}
		})
	}
}

func TestStripBackchannel_LeavesCleanUrlsAlone(t *testing.T) {
	for _, in := range []string{
		"rtsp://127.0.0.1:2001/cui_pool_main?video&audio&timeout=15",
		"rtsp://127.0.0.1:2001/cui_pool_main",
		"rtsp://user:pass@10.0.0.1:554/cam/realmonitor?channel=1&subtype=0",
	} {
		if got := stripBackchannel(in); got != in {
			t.Errorf("expected unchanged\n got: %s\nwant: %s", got, in)
		}
	}
}

// A URL this can't parse must pass through rather than break recording: a
// slightly odd URL that ffmpeg would have accepted is far better than none.
func TestStripBackchannel_PassesThroughUnparseableUrls(t *testing.T) {
	in := "rtsp://[::1 broken?backchannel=opus"
	if got := stripBackchannel(in); got != in {
		t.Fatalf("expected the input returned unchanged, got %s", got)
	}
}

// Substring safety: a parameter merely containing "backchannel" is not one.
func TestStripBackchannel_OnlyMatchesTheExactParameter(t *testing.T) {
	in := "rtsp://h/s?video&nobackchannel=1&backchannelmode=x&timeout=15"
	if got := stripBackchannel(in); got != in {
		t.Fatalf("expected unrelated params kept, got %s", got)
	}
}
