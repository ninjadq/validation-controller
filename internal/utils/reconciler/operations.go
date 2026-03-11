package reconciler

import (
	"context"
	"time"
)

var DefaultRequeueDelay = 10 * time.Second

type ReconcileOperation func(ctx context.Context) (OperationResult, error)

type OperationResult struct {
	RequeueDelay   time.Duration
	RequeueRequest bool
	CancelRequest  bool
}

func ContinueOperationResult() OperationResult {
	return OperationResult{
		RequeueDelay:   0,
		RequeueRequest: false,
		CancelRequest:  false,
	}
}

func StopOperationResult() OperationResult {
	return OperationResult{
		RequeueDelay:   0,
		RequeueRequest: false,
		CancelRequest:  true,
	}
}

func StopProcessing() (OperationResult, error) {
	return StopOperationResult(), nil
}

func Requeue() (OperationResult, error) {
	return OperationResult{
		RequeueDelay:   DefaultRequeueDelay,
		RequeueRequest: true,
		CancelRequest:  false,
	}, nil
}

func RequeueWithError(err error) (OperationResult, error) {
	return OperationResult{
		RequeueDelay:   DefaultRequeueDelay,
		RequeueRequest: true,
		CancelRequest:  false,
	}, err
}

func RequeueAfter(delay time.Duration, err error) (OperationResult, error) {
	return OperationResult{
		RequeueDelay:   delay,
		RequeueRequest: true,
		CancelRequest:  false,
	}, err
}

func ContinueProcessing() (OperationResult, error) {
	return ContinueOperationResult(), nil
}
