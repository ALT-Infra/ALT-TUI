package licenses

import (
	"strings"
	"testing"
)

func TestNoticesCoverEveryRedistributionSurface(t *testing.T) {
	t.Parallel()

	for _, required := range []string{
		"cloudwego/eino",
		"modernc.org/sqlite",
		"egui ",
		"eframe ",
		"egui_graph ",
		"Bubblewrap 0.12.0",
		"LGPL-2.0-or-later",
	} {
		if !strings.Contains(notices, required) {
			t.Errorf("third-party notices do not contain %q", required)
		}
	}
}
