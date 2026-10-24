package browser

import (
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
)

// Open launches an HTTP(S) URL in the user's default browser without waiting
// for the browser process to exit.
func Open(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("unsupported browser URL")
	}

	name, args, err := command(rawURL)
	if err != nil {
		return err
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("find browser launcher: %w", err)
	}
	process := exec.Command(path, args...)
	if err := process.Start(); err != nil {
		return fmt.Errorf("start browser launcher: %w", err)
	}
	go func() {
		_ = process.Wait()
	}()
	return nil
}

func command(rawURL string) (string, []string, error) {
	switch runtime.GOOS {
	case "darwin":
		return "open", []string{rawURL}, nil
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", rawURL}, nil
	case "linux":
		return "xdg-open", []string{rawURL}, nil
	default:
		return "", nil, fmt.Errorf("opening a browser is not supported on %s", runtime.GOOS)
	}
}
