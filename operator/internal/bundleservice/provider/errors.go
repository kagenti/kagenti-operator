package provider

import "errors"

var (
	ErrBundleTooLarge = errors.New("bundle exceeds size limit")
	ErrNotFound       = errors.New("policy not found")
)
