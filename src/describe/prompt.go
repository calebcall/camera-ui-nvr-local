package describe

import (
	"fmt"
	"sort"
	"strings"
	"time"

	sdk "github.com/cameraui/sdk/go"
)

// systemPrompt is fixed and deliberately not user-configurable. The JSON shape it
// demands is a contract that parseDescription, the stored column, and every
// frontend surface rendering a description all depend on, and a user-edited
// prompt is the easiest possible way to break that contract while looking exactly
// like a plugin defect.
//
// The framing in the first line does most of the work: telling the model it is
// looking at consecutive stills from ONE fixed camera during ONE event is what
// turns "a person, a car" into "a person walks past the car toward the door" —
// the chronological narrative the frontend actually renders. Without it, models
// reliably treat the frames as unrelated photos and describe each in isolation.
//
// The rules exist because each corresponds to a specific way these replies go
// wrong: inventing detail that isn't in frame, second-guessing identities the
// detection pipeline already resolved, reading the camera's own perspective as
// camera movement, and inflating routine activity into a threat.
const systemPrompt = `You are analyzing still frames captured in chronological order from a single fixed security camera during one motion event.

Respond with a single JSON object and nothing else. No markdown, no code fences, no commentary. The object must have exactly these keys:

- "title": a short label for the event, at most 6 words.
- "description": a chronological narrative of what happens across the frames, 2 to 4 sentences.
- "summary": at most 2 sentences, suitable for a phone notification.
- "threatLevel": an integer, 0 for normal activity, 1 for suspicious activity, 2 for a genuine threat.

Rules:
- Describe only what is visible. Do not invent details you cannot see.
- The detector's own findings are given to you; prefer its labels and identities over your own guesses.
- The camera does not move. Apparent movement is the subjects moving, not the camera.
- Do not speculate about intent beyond what the frames support. Routine activity is threatLevel 0.`

// EventContext renders the detector's own findings as the text part of the user
// message that accompanies the frames.
//
// Handing the model what the detection pipeline already knows — labels and their
// confidences, recognized faces and plate text, which zones were crossed — is
// what stops it from guessing at identifications the system has already made
// properly: a named face beats "a man", and a zone name beats "near some
// bushes". It is the same material detectionSummary (events_ingest.go) puts in a
// notification body, for the same reason.
//
// Every section except the camera is conditional, so a sparse event produces a
// short context rather than a scaffold of empty labels. The result is never
// empty: some OpenAI-compatible servers reject a request whose text part is a
// blank string.
func EventContext(cameraName string, ev sdk.DetectionEvent) string {
	var b strings.Builder

	// A blank name means the camera lookup failed (most likely a camera removed
	// between ingestion and generation). Naming that explicitly is better than
	// emitting "Camera: " with nothing after it, which invites the model to
	// invent a location.
	if cameraName == "" {
		cameraName = "unknown camera"
	}
	fmt.Fprintf(&b, "Camera: %s\n", cameraName)

	// Local time, not UTC: the model's read of a scene legitimately depends on
	// whether it is the middle of the afternoon or 3am, and the plugin already
	// runs in the user's timezone.
	if ev.StartTime > 0 {
		fmt.Fprintf(&b, "Local time: %s\n", time.UnixMilli(ev.StartTime).Format("2006-01-02 15:04:05"))
	}
	// EndTime is zero for an event that never terminated, so the comparison
	// guards against reporting a wildly negative duration rather than omitting
	// one.
	if ev.EndTime > ev.StartTime {
		fmt.Fprintf(&b, "Duration: %d seconds\n", (ev.EndTime-ev.StartTime)/1000)
	}

	if objects := rankedDetections(ev); len(objects) > 0 {
		fmt.Fprintf(&b, "Detected objects: %s\n", strings.Join(objects, ", "))
	}
	if attrs := recognizedAttributes(ev); len(attrs) > 0 {
		fmt.Fprintf(&b, "Recognized: %s\n", strings.Join(attrs, ", "))
	}
	if zones := eventZones(ev); len(zones) > 0 {
		fmt.Fprintf(&b, "Zones: %s\n", strings.Join(zones, ", "))
	}

	// A closing instruction, separated by a blank line, so the context above
	// reads as data and this reads as the request. Models otherwise sometimes
	// respond to the last line of context as if it were the question.
	b.WriteString("\nDescribe this event.")
	return b.String()
}

// EventLabels returns the event's distinct, non-empty detection labels — the
// input to the allow-list gate (Config.AllowsLabels), which is what decides
// whether an event costs an inference at all.
//
// De-duplicated because the same label repeats in every segment of a long event,
// and ordered by first appearance rather than by map iteration so callers and
// tests see a stable list. Labels are returned exactly as the detector emitted
// them, casing included; AllowsLabels normalizes at comparison time.
func EventLabels(ev sdk.DetectionEvent) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, seg := range ev.Segments {
		for _, d := range seg.Detections {
			if d.Label == "" {
				continue
			}
			if _, ok := seen[d.Label]; ok {
				continue
			}
			seen[d.Label] = struct{}{}
			out = append(out, d.Label)
		}
	}
	return out
}

// rankedDetections lists each detected label once with its best confidence,
// highest first — e.g. ["person 94%", "vehicle 81%"].
//
// Best-per-label rather than every detection, because an event carries the same
// label once per segment and repeating it adds tokens without adding
// information. Ordered by confidence so the model reads the detector's most
// certain findings first, with the label as a tiebreak to keep the output
// deterministic across runs (map iteration order is not).
func rankedDetections(ev sdk.DetectionEvent) []string {
	best := map[string]float64{}
	for _, seg := range ev.Segments {
		for _, d := range seg.Detections {
			if d.Label == "" {
				continue
			}
			if s, ok := best[d.Label]; !ok || d.Score > s {
				best[d.Label] = d.Score
			}
		}
	}

	labels := make([]string, 0, len(best))
	for label := range best {
		labels = append(labels, label)
	}
	sort.Slice(labels, func(i, j int) bool {
		if best[labels[i]] != best[labels[j]] {
			return best[labels[i]] > best[labels[j]]
		}
		return labels[i] < labels[j]
	})

	out := make([]string, 0, len(labels))
	for _, label := range labels {
		// Rendered as a rounded percentage rather than a raw float: "94%" is
		// what the model's training data looks like, and the extra precision of
		// 0.9432 carries no meaning it can act on. The +0.5 rounds rather than
		// truncates, matching detectionSummary.
		out = append(out, fmt.Sprintf("%s %d%%", label, int(best[label]*100+0.5)))
	}
	return out
}

// recognizedAttributes lists distinct recognized attribute labels — face
// identities, plate text, classifier labels — each suffixed with its type so the
// model knows "Caleb" is a face and "7ABC123" is a plate rather than guessing
// from the string's shape.
//
// De-duplicated on the rendered entry rather than the label alone, so the same
// text recognized as two different types (rare, but possible when a classifier
// and a plate reader agree on a string) survives as both.
func recognizedAttributes(ev sdk.DetectionEvent) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, seg := range ev.Segments {
		for _, a := range seg.Attributes {
			if a.Label == "" {
				continue
			}
			entry := a.Label
			if a.Type != "" {
				entry = fmt.Sprintf("%s (%s)", a.Label, a.Type)
			}
			if _, ok := seen[entry]; ok {
				continue
			}
			seen[entry] = struct{}{}
			out = append(out, entry)
		}
	}
	return out
}

// eventZones lists the distinct detection-zone names the event overlapped.
//
// Worth the tokens because zone names are user-chosen ("Driveway", "Porch",
// "Neighbour's lawn") and therefore encode intent that is invisible in the
// pixels: the same person walking is unremarkable in one zone and notable in
// another. Order follows first appearance across segments.
func eventZones(ev sdk.DetectionEvent) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, seg := range ev.Segments {
		for _, z := range seg.Zones {
			if z == "" {
				continue
			}
			if _, ok := seen[z]; ok {
				continue
			}
			seen[z] = struct{}{}
			out = append(out, z)
		}
	}
	return out
}
