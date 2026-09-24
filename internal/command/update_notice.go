package command

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/update"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

const updateNoticeFile = "update-check.json"

// emitUpdateNotice tells a user that their channel is offering a newer bundle.
// It runs after the command so a check never delays the work, writes to stderr
// so machine-readable output on stdout stays parseable, and stays silent unless
// there is something to say.
func (a *App) emitUpdateNotice(ctx context.Context, cfg config.Config, paths config.Paths, renderer output.Renderer) {
	if !updateNoticeWanted(renderer) {
		return
	}
	dir := strings.TrimSpace(paths.Dir)
	if dir == "" {
		return
	}
	latest := update.Notice(ctx, update.NoticeOptions{
		CachePath:       filepath.Join(dir, updateNoticeFile),
		BaseURL:         updateNoticeBaseURL(cfg),
		ChannelManifest: version.ChannelManifest(),
		CurrentVersion:  a.version.Version,
	})
	if latest == "" {
		return
	}
	_, _ = fmt.Fprintf(
		renderer.Err,
		"\n%s %s is available (you have %s). Run `%s update`, or set BEAM_NO_UPDATE_NOTICE=1 to silence this.\n",
		version.CLIName(), latest, displayVersion(a.version.Version), version.CLIName(),
	)
}

// updateNoticeWanted keeps the notice out of anything that would be parsed or
// that has no meaningful version to compare.
func updateNoticeWanted(renderer output.Renderer) bool {
	if renderer.Mode != output.Human || renderer.Err == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BEAM_NO_UPDATE_NOTICE"))) {
	case "1", "true", "yes", "on":
		return false
	}
	// A PR bundle follows one candidate rather than a channel, and a source
	// build has no published counterpart, so neither has an update to offer.
	if _, ok := version.PullRequestNumber(); ok {
		return false
	}
	plain := strings.TrimPrefix(strings.TrimSpace(version.Version), "v")
	return plain != "" && plain != "dev"
}

func updateNoticeBaseURL(_ config.Config) string {
	if override := strings.TrimSpace(os.Getenv("BEAM_CDN_BASE_URL")); override != "" {
		return override
	}
	return update.DefaultBaseURL
}
