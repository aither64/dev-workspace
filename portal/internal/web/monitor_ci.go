package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/processgroup"
)

// This registry owns only commands started by this portal instance. A new
// instance cannot adopt a durable running receipt or reconstruct its PID.
type ciPollController struct {
	mu    sync.Mutex
	calls map[string]*ownedCIPoll
}
type ownedCIPoll struct {
	receipt monitoroperation.PollReceipt
	cancel  context.CancelFunc
	done    chan struct{}
}
type ciPollObserver struct {
	controller *ciPollController
	claim      monitoroperation.ObserverRecord
	executable string
	publish    func(monitoroperation.PollReceipt) error
	admit      func() error
}

func (c *ciPollController) OwnsPoll(id string, receipt monitoroperation.PollReceipt) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	call := c.calls[id]
	if call == nil || call.receipt.ID != receipt.ID || call.receipt.OwnerEpoch != receipt.OwnerEpoch || !call.receipt.StartedAt.Equal(receipt.StartedAt) || receipt.Handle != "" && call.receipt.Handle != receipt.Handle {
		return false
	}
	select {
	case <-call.done:
		return call.receipt.State == "joined"
	default:
		return true
	}
}
func (c *ciPollController) Poll(context.Context, monitoroperation.Source) (monitoroperation.CISnapshot, monitoroperation.PollReceipt, error) {
	return monitoroperation.CISnapshot{}, monitoroperation.PollReceipt{}, errors.New("GitHub observer requires an exact operation binding")
}
func (c *ciPollController) Quiesce(ctx context.Context, id string) (monitoroperation.PollReceipt, error) {
	c.mu.Lock()
	call := c.calls[id]
	if call == nil {
		c.mu.Unlock()
		return monitoroperation.PollReceipt{}, errors.New("exact GitHub request handle is unavailable")
	}
	cancel, done := call.cancel, call.done
	c.mu.Unlock()
	cancel() // Only the original captured invocation's context.
	select {
	case <-ctx.Done():
		return monitoroperation.PollReceipt{}, ctx.Err()
	case <-done:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if call.receipt.State != "joined" {
		return call.receipt, errors.New("GitHub request local shutdown remains unproved")
	}
	return call.receipt, nil
}
func (o *ciPollObserver) Quiesce(ctx context.Context, id string) (monitoroperation.PollReceipt, error) {
	return o.controller.Quiesce(ctx, id)
}

func ciRunArguments(source monitoroperation.Source) ([]string, error) {
	if err := monitoroperation.ValidateDirectCISource(source); err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("repos/%s/actions/runs/%s/attempts/%d", source.Repository, source.RunID, source.RunAttempt)
	return []string{"api", "--method", "GET", "--hostname", "github.com", endpoint}, nil
}

func (o *ciPollObserver) Poll(ctx context.Context, source monitoroperation.Source) (snapshot monitoroperation.CISnapshot, receipt monitoroperation.PollReceipt, outcome error) {
	if o.claim.Poll == nil {
		return snapshot, receipt, errors.New("GitHub poll intent is missing")
	}
	receipt = *o.claim.Poll
	if o.controller == nil || o.publish == nil || o.claim.ID == "" || receipt.ID == "" || receipt.OwnerEpoch == "" || receipt.State != "intent" || o.claim.SourceDigest != monitoroperation.SourceDigest(source) || monitoroperation.SourceDigest(o.claim.Source) != o.claim.SourceDigest {
		return snapshot, receipt, errors.New("GitHub poll intent binding changed")
	}
	args, argumentErr := ciRunArguments(source)
	executable := o.executable
	if executable == "" {
		executable = "gh"
	}
	binding, _ := json.Marshal(struct {
		Operation, Poll, Epoch, Source, Executable string
		Args                                       []string
	}{o.claim.ID, receipt.ID, receipt.OwnerEpoch, o.claim.SourceDigest, executable, args})
	digest := sha256.Sum256(binding)
	receipt.Handle = "github-read:" + hex.EncodeToString(digest[:])
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	call := &ownedCIPoll{receipt: receipt, cancel: cancel, done: make(chan struct{})}
	o.controller.mu.Lock()
	if o.controller.calls == nil {
		o.controller.calls = make(map[string]*ownedCIPoll)
	}
	if previous := o.controller.calls[o.claim.ID]; previous != nil {
		select {
		case <-previous.done:
			if previous.receipt.State != "joined" || previous.receipt.ID == receipt.ID {
				o.controller.mu.Unlock()
				return snapshot, receipt, errors.New("GitHub request already has a retained handle")
			}
		default:
			o.controller.mu.Unlock()
			return snapshot, receipt, errors.New("GitHub request is already running")
		}
	}
	o.controller.calls[o.claim.ID] = call
	o.controller.mu.Unlock()
	defer func() { o.controller.mu.Lock(); call.receipt = receipt; close(call.done); o.controller.mu.Unlock() }()
	// Register before the final phase check, so Begin can cancel this exact
	// admitted owner even at the claim-to-Start boundary. Lost maps prove nothing.
	var admissionErr error
	if o.admit != nil {
		admissionErr = o.admit()
	}
	if err := errors.Join(argumentErr, admissionErr); err != nil {
		// This exact registered producer has not called Start. Keep its bound
		// handle and publish positive unsent proof through the normal commit.
		// Missing, conflicting or lost ownership returned before registration.
		receipt.State = "joined"
		finished := time.Now().UTC()
		receipt.FinishedAt = &finished
		return snapshot, receipt, err
	}
	command := exec.Command(executable, args...)
	result, runErr := processgroup.RunDetailed(callCtx, command, func() error {
		receipt.State = "running"
		o.controller.mu.Lock()
		call.receipt = receipt
		o.controller.mu.Unlock()
		return o.publish(receipt)
	})
	if !result.Started {
		receipt.State = "joined" // The original producer positively refused Start.
		finished := time.Now().UTC()
		receipt.FinishedAt = &finished
	} else if ciRequestJoined(result, command) {
		receipt.State = "joined"
		receipt.FinishedAt = &result.FinishedAt
		status := command.ProcessState.ExitCode()
		receipt.ExitStatus = &status
	} else {
		receipt.State = "running" // Retain the durable effects fence, even after Wait.
		return snapshot, receipt, errors.Join(runErr, errors.New("GitHub request local shutdown remains unproved"))
	}
	if runErr != nil {
		return snapshot, receipt, fmt.Errorf("GitHub status request: %w", runErr)
	}
	if result.OutputTruncated {
		return snapshot, receipt, errors.New("GitHub status response exceeds its bound")
	}
	snapshot, err := decodeCIRun(result.Output, source)
	return snapshot, receipt, err
}

func ciRequestJoined(result processgroup.Result, command *exec.Cmd) bool {
	if !result.Started || !result.WaitReturned || command.ProcessState == nil || !result.PipesComplete || result.PipeErr != nil || !result.GroupAbsent || errors.Is(result.WaitErr, exec.ErrWaitDelay) {
		return false
	}
	if result.CancelErr != nil && !errors.Is(result.CancelErr, syscall.ESRCH) || result.CleanupErr != nil && !errors.Is(result.CleanupErr, syscall.ESRCH) {
		return false
	}
	var exited *exec.ExitError
	return result.WaitErr == nil || errors.As(result.WaitErr, &exited)
}

func decodeCIRun(data []byte, source monitoroperation.Source) (monitoroperation.CISnapshot, error) {
	var response struct {
		ID         json.Number `json:"id"`
		WorkflowID json.Number `json:"workflow_id"`
		Attempt    int         `json:"run_attempt"`
		Head       string      `json:"head_sha"`
		Status     string      `json:"status"`
		Conclusion string      `json:"conclusion"`
		Repository struct {
			Name string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return monitoroperation.CISnapshot{}, errors.New("GitHub status response is invalid")
	}
	if string(response.ID) != source.RunID || string(response.WorkflowID) != source.WorkflowID || response.Attempt != source.RunAttempt || response.Head != source.Commit || response.Repository.Name != source.Repository {
		return monitoroperation.CISnapshot{}, errors.New("GitHub status response source changed")
	}
	switch response.Status {
	case "queued", "in_progress", "waiting", "requested", "pending":
		if response.Conclusion != "" {
			return monitoroperation.CISnapshot{}, errors.New("GitHub status response is inconsistent")
		}
	case "completed":
		switch response.Conclusion {
		case "success", "failure", "cancelled", "skipped", "timed_out", "action_required", "neutral", "stale", "startup_failure":
		default:
			return monitoroperation.CISnapshot{}, errors.New("GitHub terminal conclusion is unavailable")
		}
	default:
		return monitoroperation.CISnapshot{}, errors.New("GitHub status response is unavailable")
	}
	return monitoroperation.CISnapshot{Status: response.Status, Conclusion: response.Conclusion, ObservedAt: time.Now().UTC(), URL: fmt.Sprintf("https://github.com/%s/actions/runs/%s/attempts/%d", source.Repository, source.RunID, source.RunAttempt), SourceDigest: monitoroperation.SourceDigest(source)}, nil
}
