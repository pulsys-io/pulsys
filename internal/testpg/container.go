// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package testpg

import (
	"context"
	"fmt"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpg "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startTestContainer launches a Postgres container running `image`,
// waits for readiness, and returns its DSN + a teardown closure.
//
// The container runs the bundled postgres user as superuser (default
// for the official image), so CREATE DATABASE TEMPLATE works out of
// the box.
func startTestContainer(image string) (string, func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	c, err := tcpg.Run(ctx, image,
		tcpg.WithDatabase("pulsys"),
		tcpg.WithUsername("pulsys"),
		tcpg.WithPassword("pulsys"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return "", nil, fmt.Errorf("testcontainers: postgres run: %w", err)
	}

	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = c.Terminate(ctx)
		return "", nil, fmt.Errorf("testcontainers: connection string: %w", err)
	}

	teardown := func() {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel2()
		_ = c.Terminate(ctx2)
	}
	return dsn, teardown, nil
}
