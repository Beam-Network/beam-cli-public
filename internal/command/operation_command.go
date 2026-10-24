package command

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
)

func (a *App) transferCommand(ctx context.Context, args []string, cfg config.Config, renderer output.Renderer) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	client, err := a.readyLocalClient(ctx, cfg)
	if err != nil {
		return err
	}
	switch args[0] {
	case "upload", "download", "duplex":
		return createTransferCommand(ctx, args[0], args[1:], client, renderer)
	case "list":
		if len(args) != 1 {
			return usage("transfer list does not accept arguments")
		}
		return listOperations(ctx, client, renderer, true)
	case "show":
		if len(args) != 2 {
			return usage("transfer show requires one operation ID")
		}
		return tunnelOperation(ctx, args[1:], client, renderer)
	case "watch":
		return watchOperation(ctx, args[1:], client, renderer)
	case "cancel":
		if len(args) != 2 {
			return usage("transfer cancel requires one operation ID")
		}
		return tunnelCancelOperation(ctx, args[1:], client, renderer)
	case "retry":
		return retryOperation(ctx, args[1:], client, renderer)
	case "logs":
		return operationLogs(ctx, args[1:], client, renderer)
	default:
		return usage(fmt.Sprintf("unknown transfer command %q", args[0]))
	}
}

func (a *App) operationCommand(ctx context.Context, args []string, cfg config.Config, renderer output.Renderer) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	client, err := a.readyLocalClient(ctx, cfg)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return usage("operation list does not accept arguments")
		}
		return listOperations(ctx, client, renderer, false)
	case "show":
		if len(args) != 2 {
			return usage("operation show requires one operation ID")
		}
		return tunnelOperation(ctx, args[1:], client, renderer)
	case "watch":
		return watchOperation(ctx, args[1:], client, renderer)
	case "cancel":
		if len(args) != 2 {
			return usage("operation cancel requires one operation ID")
		}
		return tunnelCancelOperation(ctx, args[1:], client, renderer)
	case "retry":
		return retryOperation(ctx, args[1:], client, renderer)
	case "logs":
		return operationLogs(ctx, args[1:], client, renderer)
	case "prune":
		if len(args) != 1 {
			return usage("operation prune does not accept arguments")
		}
		result, err := client.PruneOperations(ctx)
		if err != nil {
			return mapTunnelError(err)
		}
		return renderer.Result(result, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "Pruned %d terminal operation(s).\n", len(result.RemovedIDs))
			return err
		})
	default:
		return usage(fmt.Sprintf("unknown operation command %q", args[0]))
	}
}

func (a *App) studioCommand(ctx context.Context, args []string, cfg config.Config, renderer output.Renderer) error {
	client, err := a.readyLocalClient(ctx, cfg)
	if err != nil {
		return err
	}
	return a.tunnelStudio(ctx, args, client, renderer)
}

func createTransferCommand(ctx context.Context, operationType string, args []string, client tunnel.Client, renderer output.Renderer) error {
	wait := has(args, "--wait") || (renderer.Interactive && !has(args, "--detach"))
	clean := withoutArgs(args, "--wait", "--detach")
	if err := validateOptions(clean, operationValueFlags, nil); err != nil {
		return err
	}
	positionals, err := positional(clean)
	if err != nil || len(positionals) != 1 {
		return usage("transfer " + operationType + " requires one file path")
	}
	file, err := filepath.Abs(positionals[0])
	if err != nil {
		return usage("operation file path is invalid")
	}
	if operationType == "upload" {
		info, statErr := os.Stat(file)
		if statErr != nil || !info.Mode().IsRegular() {
			return usage("uploaded file must exist and be a regular file")
		}
	}
	request := tunnel.OperationRequest{Type: operationType, File: file}
	if err := populateTransferOptions(clean, &request); err != nil {
		return err
	}
	if operationType != "duplex" && request.Session == "" && request.WorkloadID == "" {
		return usage("transfer " + operationType + " requires --session or --workload-id")
	}
	if err := ensureCompatible(ctx, client); err != nil {
		return err
	}
	operation, err := client.CreateOperation(ctx, request)
	if err != nil {
		return mapTunnelError(err)
	}
	if wait {
		return watchOperationID(ctx, operation.ID, client, renderer, operation)
	}
	return renderOperation(operation, renderer)
}

func listOperations(ctx context.Context, client tunnel.Client, renderer output.Renderer, transfersOnly bool) error {
	if err := ensureCompatible(ctx, client); err != nil {
		return err
	}
	result, err := client.Operations(ctx)
	if err != nil {
		return mapTunnelError(err)
	}
	if transfersOnly {
		filtered := result.Operations[:0]
		for _, operation := range result.Operations {
			if operation.Type == "upload" || operation.Type == "download" || operation.Type == "duplex" {
				filtered = append(filtered, operation)
			}
		}
		result.Operations = filtered
	}
	return renderer.Result(result, func(w io.Writer) error {
		rows := make([][]string, 0, len(result.Operations))
		for _, operation := range result.Operations {
			rows = append(rows, []string{operation.ID, operation.Type, operation.Status})
		}
		return renderer.Table(w, output.Table{
			Headers: []string{"OPERATION ID", "TYPE", "STATUS"},
			Rows:    rows,
			Style:   output.RowStyle(2),
		})
	})
}

func watchOperation(ctx context.Context, args []string, client tunnel.Client, renderer output.Renderer) error {
	if len(args) != 1 {
		return usage("operation watch requires one operation ID")
	}
	return watchOperationID(ctx, args[0], client, renderer, tunnel.Operation{})
}

func watchOperationID(ctx context.Context, id string, client tunnel.Client, renderer output.Renderer, initial tunnel.Operation) error {
	previous := ""
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	operation := initial
	for {
		if operation.ID == "" {
			var err error
			operation, err = client.Operation(ctx, id)
			if err != nil {
				return mapTunnelError(err)
			}
		}
		encoded, _ := json.Marshal(operation)
		if string(encoded) != previous {
			if renderer.Mode == output.JSON {
				if err := json.NewEncoder(renderer.Out).Encode(operation); err != nil {
					return err
				}
			} else if renderer.Mode == output.Human {
				_, _ = fmt.Fprintf(renderer.Out, "%s\t%s\t%s\n", operation.ID, operation.Type, operation.Status)
			}
			previous = string(encoded)
		}
		if terminalOperation(operation.Status) {
			if operation.Status == "failed" {
				return cliError(ExitOperationFailed, "Operation failed.", operationFailure(operation), nil)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			operation = tunnel.Operation{}
		}
	}
}

func retryOperation(ctx context.Context, args []string, client tunnel.Client, renderer output.Renderer) error {
	if len(args) != 1 {
		return usage("operation retry requires one operation ID")
	}
	previous, err := client.Operation(ctx, args[0])
	if err != nil {
		return mapTunnelError(err)
	}
	if !terminalOperation(previous.Status) {
		return cliError(ExitConflict, "Operation is still active.", "Watch or cancel it before retrying.", nil)
	}
	retried, err := client.CreateOperation(ctx, previous.Request)
	if err != nil {
		return mapTunnelError(err)
	}
	return renderOperation(retried, renderer)
}

func operationLogs(ctx context.Context, args []string, client tunnel.Client, renderer output.Renderer) error {
	if len(args) < 1 || len(args) > 2 || (len(args) == 2 && args[1] != "--follow") {
		return usage("operation logs requires one operation ID and optional --follow")
	}
	id := args[0]
	follow := len(args) == 2
	if err := ensureCompatible(ctx, client); err != nil {
		return err
	}
	// Resolve the operation first. The log feed is filtered client-side, so an
	// unknown ID would otherwise match nothing and report success, while every
	// sibling command rejects it.
	if _, err := client.Operation(ctx, id); err != nil {
		return mapTunnelError(err)
	}
	return client.Logs(ctx, follow, func(event tunnel.LogEvent) error {
		operationID, _ := event.Fields["operation_id"].(string)
		if operationID != id {
			return nil
		}
		if renderer.Mode == output.JSON {
			return json.NewEncoder(renderer.Out).Encode(event)
		}
		if renderer.Mode == output.Human {
			_, err := fmt.Fprintf(renderer.Out, "%s\t%s\t%s\n", event.Time.Format("15:04:05"), event.Level, event.Message)
			return err
		}
		return nil
	})
}

func terminalOperation(status string) bool {
	switch strings.ToLower(status) {
	case "succeeded", "completed", "failed", "cancelled", "expired", "rejected":
		return true
	default:
		return false
	}
}

func operationFailure(operation tunnel.Operation) string {
	if operation.Error != nil {
		return firstNonEmpty(operation.Error.Message, operation.Error.Code)
	}
	return "Inspect `beam operation logs " + operation.ID + "`."
}

func withoutArgs(args []string, removed ...string) []string {
	remove := map[string]bool{}
	for _, arg := range removed {
		remove[arg] = true
	}
	result := make([]string, 0, len(args))
	for _, arg := range args {
		if !remove[arg] {
			result = append(result, arg)
		}
	}
	return result
}
