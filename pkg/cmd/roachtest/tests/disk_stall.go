// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package tests

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/cluster"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/registry"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/test"
	"github.com/cockroachdb/cockroach/pkg/roachprod/install"
	"github.com/cockroachdb/cockroach/pkg/util/contextutil"
	"github.com/cockroachdb/cockroach/pkg/util/retry"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
)

// RegisterDiskStalledDetection registers the disk stall test.
func RegisterDiskStalledDetection(r registry.Registry) {
	for _, affectsLogDir := range []bool{false, true} {
		for _, affectsDataDir := range []bool{false, true} {
			// Grab copies of the args because we'll pass them into a closure.
			// Everyone's favorite bug to write in Go.
			affectsLogDir := affectsLogDir
			affectsDataDir := affectsDataDir
			r.Add(registry.TestSpec{
				Name: fmt.Sprintf(
					"disk-stalled/log=%t,data=%t",
					affectsLogDir, affectsDataDir,
				),
				Owner:   registry.OwnerStorage,
				Cluster: r.MakeClusterSpec(1),
				Run: func(ctx context.Context, t test.Test, c cluster.Cluster) {
					runDiskStalledDetection(ctx, t, c, affectsLogDir, affectsDataDir)
				},
			})
		}
	}
}

func runDiskStalledDetection(
	ctx context.Context, t test.Test, c cluster.Cluster, affectsLogDir bool, affectsDataDir bool,
) {
	if c.IsLocal() && runtime.GOOS != "linux" {
		t.Fatalf("must run on linux os, found %s", runtime.GOOS)
	}
	n := c.Node(1)

	// Create a loop device on the node to which we can then attach the delay.
	t.Status("configuring mounts")
	c.Run(ctx, n, "dd if=/dev/zero of={store-dir}/faulty_fs bs=1M count=1k")
	c.Run(ctx, n, "mkfs.ext4 {store-dir}/faulty_fs")
	res, err := c.RunWithDetailsSingleNode(ctx, t.L(), n, "sudo losetup -f --show {store-dir}/faulty_fs")
	if err != nil {
		t.Fatal(err)
	}
	dev := strings.TrimSpace(res.Stdout)

	res, err = c.RunWithDetailsSingleNode(ctx, t.L(), n, fmt.Sprintf("sudo blockdev --getsz %s", dev))
	if err != nil {
		t.Fatal(err)
	}
	devSize := strings.TrimSpace(res.Stdout)

	// Set up the delayed FS on top of the loop device.
	cmd := fmt.Sprintf(
		`echo "0 %s delay %s 0 0" | sudo dmsetup create delayed`,
		devSize, dev,
	)
	c.Run(ctx, n, cmd)

	// Mount the delayed FS.
	c.Run(ctx, n, "sudo umount -f {store-dir}/faulty || true")
	c.Run(ctx, n, "mkdir -p {store-dir}/{real,faulty} || true")
	c.Run(ctx, n, "sudo mount /dev/mapper/delayed {store-dir}/faulty")

	c.Run(ctx, n, "sudo mkdir -p {store-dir}/real/logs")
	c.Run(ctx, n, "sudo chmod -R 777 {store-dir}/{real,faulty}")

	// Make sure the actual logs are downloaded as artifacts.
	c.Run(ctx, n, "rm -f logs && ln -s {store-dir}/real/logs logs || true")

	errCh := make(chan install.RunResultDetails)

	// Lower the default max log and engine sync times. This allows the test to
	// fail faster.
	const maxSync = 10 * time.Second

	logDir := "real/logs"
	if affectsLogDir {
		logDir = "faulty/logs"
	}
	dataDir := "real"
	if affectsDataDir {
		dataDir = "faulty"
	}

	tStarted := timeutil.Now()
	dur := 10 * time.Minute
	if !affectsDataDir && !affectsLogDir {
		dur = 30 * time.Second
	}

	go func() {
		t.WorkerStatus("running server")
		result, err := c.RunWithDetailsSingleNode(ctx, t.L(), n,
			fmt.Sprintf("timeout --signal 9 %ds env "+
				"COCKROACH_ENGINE_MAX_SYNC_DURATION_DEFAULT=%s "+
				"COCKROACH_LOG_MAX_SYNC_DURATION=%s "+
				"COCKROACH_AUTO_BALLAST=false "+
				"./cockroach start-single-node --insecure --store {store-dir}/%s "+
				"--log '{sinks: {stderr: {filter: INFO}}, file-defaults: {dir: \"{store-dir}/%s\"}}'",
				int(dur.Seconds()), maxSync, maxSync, dataDir, logDir,
			),
		)
		if err != nil {
			result.Err = err
		}
		errCh <- result
	}()

	t.Status("waiting for node to become ready")
	const readyTimeout = 30 * time.Second
	err = contextutil.RunWithTimeout(ctx, "wait-for-ready", readyTimeout, func(ctx context.Context) error {
		conn := c.Conn(ctx, t.L(), 1)
		return retry.ForDuration(readyTimeout, func() error {
			return WaitForReplication(ctx, t, conn, 1)
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		t.Status("blocking storage")
		ioWaitTime := (2 * maxSync).Milliseconds()
		cmd = fmt.Sprintf(
			`echo "0 %s delay %s 0 %d" | sudo dmsetup reload delayed &&`+
				`sudo dmsetup resume delayed`,
			devSize, dev, ioWaitTime,
		)
		c.Run(ctx, n, cmd)
	}()

	result := <-errCh
	if result.Err == nil {
		t.Fatalf("expected an error: %s", result.Stdout)
	}

	// This test can also run in sanity check mode to make sure it doesn't fail
	// due to the aggressive env vars above.
	expectMsg := affectsDataDir || affectsLogDir

	if expectMsg != strings.Contains(result.Stderr, "disk stall detected") {
		t.Fatalf("unexpected output: %v", result.Err)
	} else if elapsed := timeutil.Since(tStarted); !expectMsg && elapsed < dur {
		t.Fatalf("no disk stall injected, but process terminated too early after %s (expected >= %s)", elapsed, dur)
	}

	t.Status("unblocking storage")
	cmd = fmt.Sprintf(
		`echo "0 %s delay %s 0 0" | sudo dmsetup reload delayed && `+
			`sudo dmsetup resume delayed`,
		devSize, dev,
	)
	c.Run(ctx, n, cmd)

	t.Status("cleaning up")
	c.Run(ctx, n, "sudo umount {store-dir}/faulty")
	c.Run(ctx, n, "sudo dmsetup remove delayed")
	c.Run(ctx, n, fmt.Sprintf("sudo losetup -d %s", dev))
	c.Run(ctx, n, "rm {store-dir}/faulty_fs")
}
