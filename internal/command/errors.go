package command

import (
	"errors"
	"fmt"
)

const (
	ExitOK                  = 0
	ExitUsage               = 2
	ExitConfig              = 3
	ExitAuth                = 4
	ExitRegistryUnavailable = 5
	ExitDaemonUnavailable   = 6
	ExitVersionIncompatible = 7
	ExitNotFound            = 8
	ExitConflict            = 9
	ExitOperationFailed     = 10
	ExitInterrupted         = 130
)

const (
	errorKindSetupModeRequired       = "setup_mode_required"
	errorKindOrganizationRequired    = "organization_required"
	errorKindOrganizationSelection   = "organization_selection_required"
	errorKindOrganizationNotFound    = "organization_not_found"
	errorKindOrganizationUnavailable = "organization_unavailable"
	errorKindAccountUnavailable      = "account_unavailable"
	errorKindAgentConflict           = "agent_configuration_conflict"
	errorKindAgentRevoked            = "agent_revoked"
	errorKindInvitationInvalid       = "invitation_invalid"
	errorKindInvitationExpired       = "invitation_expired"
	errorKindInvitationRevoked       = "invitation_revoked"
	errorKindInvitationConsumed      = "invitation_consumed"
	errorKindInvitationRoomMismatch  = "invitation_room_mismatch"
)

type Error struct {
	Code    int    `json:"code"`
	Kind    string `json:"kind,omitempty"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	Cause   error  `json:"-"`
}

func (e *Error) Error() string {
	if e.Cause == nil {
		return e.Message
	}
	return fmt.Sprintf("%s: %v", e.Message, e.Cause)
}

func (e *Error) Unwrap() error { return e.Cause }

func cliError(code int, message, hint string, cause error) *Error {
	return &Error{Code: code, Message: message, Hint: hint, Cause: cause}
}

func typedCLIError(code int, kind, message, hint string, cause error) *Error {
	return &Error{Code: code, Kind: kind, Message: message, Hint: hint, Cause: cause}
}

func asCLIError(err error) *Error {
	var result *Error
	if errors.As(err, &result) {
		return result
	}
	return cliError(ExitOperationFailed, "The command failed.", "Run with valid inputs and try again.", err)
}
