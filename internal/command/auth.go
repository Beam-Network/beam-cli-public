package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/beamapi"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
)

func (a *App) authentication(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		return renderNamedHelp("auth", renderer)
	}
	store := a.authStore(paths.Credentials)
	session := &auth.Session{
		Client: auth.DeviceClient{BaseURL: cfg.AuthURL},
		Store:  store,
	}
	switch args[0] {
	case "login":
		if len(args) != 1 {
			return usage("auth login does not accept arguments")
		}
		return a.authLogin(ctx, cfg, session, renderer)
	case "logout":
		if len(args) != 1 {
			return usage("auth logout does not accept arguments")
		}
		return authLogout(ctx, session, renderer)
	case "whoami":
		if len(args) != 1 {
			return usage("auth whoami does not accept arguments")
		}
		return authWhoami(ctx, cfg, session, renderer)
	default:
		return usage(fmt.Sprintf("unknown auth command %q", args[0]))
	}
}

func (a *App) authLogin(ctx context.Context, cfg config.Config, session *auth.Session, renderer output.Renderer) error {
	start, err := session.Client.Start(ctx)
	if err != nil {
		return cliError(ExitAuth, "Could not start Beam authentication.", "Check BEAM_AUTH_URL and your network connection.", err)
	}
	verificationURL := start.VerificationURIComplete
	if renderer.Mode == output.Human && renderer.Interactive && a.openURL != nil {
		if err := a.openURL(verificationURL); err == nil {
			renderer.Progress("Opening your browser to sign in to Beam...")
		} else {
			renderer.Progress("Your browser could not be opened automatically.")
		}
		renderer.Progress("")
		renderer.Progress("  Link: " + renderer.Hyperlink(verificationURL))
		renderer.Progress("  Code: " + start.UserCode)
		renderer.Progress("")
	} else {
		renderer.Progress("Open Beam in your browser: " + renderer.Hyperlink(verificationURL))
		renderer.Progress("Code: " + start.UserCode)
	}
	if renderer.Mode == output.JSON {
		_ = json.NewEncoder(renderer.Err).Encode(map[string]any{
			"event":                     "device_authorization",
			"verification_uri":          start.VerificationURI,
			"verification_uri_complete": start.VerificationURIComplete,
			"user_code":                 start.UserCode,
		})
	}
	spinner := renderer.StartSpinner("Waiting for authorization")
	token, err := session.Client.Authorize(ctx, start)
	spinner.Stop()
	if err != nil {
		var oauthErr *auth.OAuthError
		if errors.As(err, &oauthErr) {
			switch oauthErr.Code {
			case "access_denied":
				return cliError(ExitAuth, "Beam login was denied.", `Run "beam auth login" to try again.`, err)
			case "expired_token":
				return cliError(ExitAuth, "Beam authentication expired.", `Run "beam auth login" again.`, err)
			}
		}
		return cliError(ExitAuth, "Beam authentication failed.", "Restart login and complete the device flow.", err)
	}
	if err := session.AcceptLogin(token); err != nil {
		return cliError(ExitConfig, "Could not save Beam credentials.", "Check the OS credential store and Beam config directory permissions.", err)
	}
	api := beamapi.Client{BaseURL: cfg.APIURL, Tokens: session}
	user, organizations, err := api.Context(ctx)
	if err != nil {
		mapped := asCLIError(mapBeamAPIError(err))
		hint := "Your Beam login was saved. " + mapped.Hint
		return typedCLIError(mapped.Code, mapped.Kind, mapped.Message, strings.TrimSpace(hint), err)
	}
	if err := session.Store.UpdateContext(user, organizations); err != nil {
		return cliError(ExitConfig, "Could not save the Beam account context.", "Check the Beam config directory permissions.", err)
	}
	return renderer.Result(map[string]any{
		"authenticated": true,
		"user":          user,
		"organizations": organizations,
	}, func(w io.Writer) error {
		if user != nil && user.Email != "" {
			_, err := fmt.Fprintf(w, "✓ Logged in to Beam as %s.\n", user.Email)
			return err
		}
		_, err := fmt.Fprintln(w, "✓ Logged in to Beam.")
		return err
	})
}

func authLogout(ctx context.Context, session *auth.Session, renderer output.Renderer) error {
	revokeErr, deleteErr := session.Logout(ctx)
	if deleteErr != nil {
		return cliError(ExitConfig, "Could not remove local credentials.", "Check the OS credential store and Beam config directory permissions.", deleteErr)
	}
	if revokeErr != nil {
		renderer.Progress("Local credentials were removed, but server revocation could not be confirmed.")
	}
	return renderer.Result(map[string]any{
		"logged_out":           true,
		"revocation_confirmed": revokeErr == nil,
	}, func(w io.Writer) error {
		_, err := fmt.Fprintln(w, "Logged out of Beam.")
		return err
	})
}

func authWhoami(ctx context.Context, cfg config.Config, session *auth.Session, renderer output.Renderer) error {
	api := beamapi.Client{BaseURL: cfg.APIURL, Tokens: session}
	user, organizations, err := api.Context(ctx)
	if err != nil {
		return mapBeamAPIError(err)
	}
	if err := session.Store.UpdateContext(user, organizations); err != nil {
		return cliError(ExitConfig, "Could not save the Beam account context.", "Check the Beam config directory permissions.", err)
	}
	result := map[string]any{
		"authenticated": true,
		"auth_url":      cfg.AuthURL,
		"api_url":       cfg.APIURL,
		"user":          user,
		"organizations": organizations,
	}
	return renderer.Result(result, func(w io.Writer) error {
		switch {
		case user != nil && user.Email != "":
			_, err = fmt.Fprintln(w, user.Email)
		case user != nil && user.Name != "":
			_, err = fmt.Fprintln(w, user.Name)
		default:
			_, err = fmt.Fprintln(w, "Authenticated with Beam.")
		}
		return err
	})
}

func mapBeamAPIError(err error) error {
	var apiErr *beamapi.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Kind {
		case beamapi.ErrAuth:
			return cliError(ExitAuth, "Beam authentication is missing or expired.", `Run "beam auth login".`, err)
		case beamapi.ErrUnavailable:
			return cliError(ExitOperationFailed, "Beam API is temporarily unavailable.", "Try again without logging out.", err)
		}
	}
	return cliError(ExitOperationFailed, "Could not load the Beam account context.", "Try again or check BEAM_API_URL.", err)
}
