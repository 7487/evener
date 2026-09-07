package hub

import (
	"context"
	"errors"
	"strings"

	"primeradiant.com/evener/appwire"
	"primeradiant.com/evener/cmd/evener-hub/internal/appsource"
	"primeradiant.com/evener/cmd/evener-hub/internal/hubcore"
)

func sourceForThread(sources *appsource.Registry, ref, threadID string) (appsource.Source, error) {
	ref = strings.TrimSpace(ref)
	threadID = strings.TrimSpace(threadID)
	if sources == nil {
		return nil, errors.New("source registry unavailable")
	}
	if ref != "" {
		source, err := sources.SourceForRef(ref)
		if err != nil {
			if _, parseErr := appwire.ParseRef(ref); parseErr != nil {
				return nil, appwire.InvalidParams(parseErr.Error())
			}
			return nil, err
		}
		return source, nil
	}
	source, ok := sources.Source("local")
	if !ok {
		return nil, errors.New("source not found: local")
	}
	if threadID == "" {
		return source, nil
	}
	return source, nil
}

func sourceForThreadWithDeletionFence(cfg hubcore.WebConfig, sources *appsource.Registry, ref, threadID string) (appsource.Source, error) {
	return withDeletionTargetOwnership(context.Background(), cfg, ref, threadID, "", func() (appsource.Source, error) {
		return sourceForThread(sources, ref, threadID)
	})
}

func withDeletionTargetOwnership[R any](
	ctx context.Context,
	cfg hubcore.WebConfig,
	ref, threadID, clientMutationID string,
	action func() (R, error),
) (R, error) {
	epoch := sessionRecoveryState(cfg, ref, threadID).Epoch
	unlock := lockDeletionTarget(cfg, ref, threadID)
	defer unlock()
	if err := deletionFenceError(cfg, ref, threadID, clientMutationID); err != nil {
		var zero R
		return zero, err
	}
	if clientMutationID != "" {
		if err := sessionActionRecoveryError(cfg, ref, threadID, epoch); err != nil {
			var zero R
			return zero, err
		}
		if err := daemonRestartRequiredError(ctx, cfg, ref, threadID, clientMutationID); err != nil {
			var zero R
			return zero, err
		}
	}
	result, err := action()
	if clientMutationID != "" && daemonOwnershipMayHaveChanged(err) {
		if restartErr := refreshDaemonRestartRequiredError(ctx, cfg, ref, threadID, clientMutationID); restartErr != nil {
			var zero R
			return zero, restartErr
		}
	}
	return result, err
}

// withSessionActionOwnership guards actions that have no durable mutation ID.
// Reads share deletion locking but must remain available for incompatible owners.
func withSessionActionOwnership[R any](ctx context.Context, cfg hubcore.WebConfig, ref, threadID string, action func() (R, error)) (R, error) {
	epoch := sessionRecoveryState(cfg, ref, threadID).Epoch
	return withDeletionTargetOwnership(ctx, cfg, ref, threadID, "", func() (R, error) {
		if err := sessionActionRecoveryError(cfg, ref, threadID, epoch); err != nil {
			var zero R
			return zero, err
		}
		if err := daemonRestartRequiredError(ctx, cfg, ref, threadID, ""); err != nil {
			var zero R
			return zero, err
		}
		result, err := action()
		if daemonOwnershipMayHaveChanged(err) {
			if restartErr := refreshDaemonRestartRequiredError(ctx, cfg, ref, threadID, ""); restartErr != nil {
				var zero R
				return zero, restartErr
			}
		}
		return result, err
	})
}

func daemonOwnershipMayHaveChanged(err error) bool {
	var initialization appsource.DaemonInitializeError
	var mismatch appwire.ProtocolVersionMismatchError
	return isSessionUnavailableError(err) || errors.As(err, &mismatch) || errors.As(err, &initialization)
}

func lockDeletionTarget(cfg hubcore.WebConfig, ref, threadID string) func() {
	if cfg.ResumeLocks == nil {
		return func() {}
	}
	threadID = deletionThreadID(ref, threadID)
	if threadID == "" {
		return func() {}
	}
	lock := cfg.ResumeLocks.For(threadID)
	lock.Lock()
	return lock.Unlock
}

func deletionFenceError(cfg hubcore.WebConfig, ref, threadID, clientMutationID string) error {
	if cfg.DeletionStore == nil {
		return nil
	}
	if _, deleted := cfg.DeletionStore.TargetState(ref, threadID); !deleted {
		return nil
	}
	if ref == "" {
		ref = localAppRef(threadID)
	}
	return appwire.WireError{
		Code:    appwire.CodeUnavailable,
		Message: "target has been deleted: " + ref,
		Data: appwire.ErrorData{
			EvenerErrorInfo:  appwire.ErrorActionUnavailable,
			ClientMutationID: clientMutationID,
			MutationOutcome:  appwire.MutationOutcomeTargetDeleted,
			RetryDisposition: appwire.RetryDispositionNone,
		},
	}
}

func isTargetDeletedError(err error) bool {
	var wireErr appwire.WireError
	if !errors.As(err, &wireErr) {
		return false
	}
	data, ok := wireErr.Data.(appwire.ErrorData)
	return ok && data.MutationOutcome == appwire.MutationOutcomeTargetDeleted
}

func deletionThreadID(ref, threadID string) string {
	if parsed, err := appwire.ParseRef(ref); err == nil && parsed.SourceID == "local" {
		return parsed.ThreadID
	}
	return threadID
}

// hubKnowsRef reports whether ref names a thread tracked in the local past
// index. It gates the retry that resumes a local past session after a live
// action reports that its daemon is unavailable.
//
// This fans out to 6 unrelated RPC handlers across the hub package (compact,
// model, vision-model, session-resume, plus its own callers here), none of
// which currently thread a request context this deep — passing
// context.Background() here (rather than widening every one of those call
// chains for one existence check) means the bounded delegate-journal scan
// still applies, just without real cancellation on this specific path.
func hubKnowsRef(cfg hubcore.WebConfig, ref string) bool {
	_, ok, _ := pastThreadForRead(context.Background(), cfg, appwire.ThreadReadParams{Ref: ref})
	return ok
}

func sessionRecoveryState(cfg hubcore.WebConfig, ref, threadID string) hubcore.SessionRecoveryState {
	if ref != "" {
		parsed, err := appwire.ParseRef(ref)
		if err != nil || parsed.SourceID != "local" {
			return hubcore.SessionRecoveryState{}
		}
	}
	if cfg.ResumeLocks == nil {
		return hubcore.SessionRecoveryState{}
	}
	id := deletionThreadID(ref, threadID)
	if id == "" {
		return hubcore.SessionRecoveryState{}
	}
	return cfg.ResumeLocks.RecoveryState(id)
}

// sessionRecoveryAdmissionError is terminal for an action already rejected by
// recovery, even if another request explicitly resumes before retry routing.
// Unwrap preserves the existing wire-level action-unavailable response.
type sessionRecoveryAdmissionError struct{ appwire.WireError }

func (err sessionRecoveryAdmissionError) Unwrap() error { return err.WireError }

func isSessionRecoveryAdmissionError(err error) bool {
	_, ok := errors.AsType[sessionRecoveryAdmissionError](err)
	return ok
}

func sessionActionRecoveryError(cfg hubcore.WebConfig, ref, threadID string, epoch uint64) error {
	state := sessionRecoveryState(cfg, ref, threadID)
	if state.Stopping > 0 || state.ResumeRequired {
		return sessionRecoveryAdmissionError{appwire.Unavailable("session recovery requires an explicit thread/resume before submitting another action")}
	}
	if state.Epoch != epoch {
		return sessionRecoveryAdmissionError{appwire.Unavailable("session recovery canceled this pending action; submit it again")}
	}
	return nil
}
