//go:build !linux && !darwin

package main

import "context"

func findPaseoSupervisor(context.Context, string) (*paseoSupervisor, error) { return nil, nil }

func restartPaseoSupervisor(context.Context, *paseoSupervisor) error { return nil }
