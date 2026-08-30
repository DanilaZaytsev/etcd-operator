/*
Copyright 2023 Timofey Larkin.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
)

// PruneTimeout bounds a prune run. Deleting one object is a single request, so
// the generous window is only there to keep a black-holed endpoint from hanging
// the Job rather than failing it.
const PruneTimeout = 5 * time.Minute

// RunPrune deletes the single stored snapshot this Job was given, and nothing
// else.
//
// It addresses exactly one object — the key derived from SNAPSHOT_NAME and the
// destination prefix, which is the same derivation the upload used. It never
// lists the bucket or the directory. A destination prefix is frequently shared
// (one bucket for many clusters, or with backups this operator did not take),
// and "delete everything under this prefix older than N" is how a retention
// policy eats somebody else's data. The caller decides what to prune by holding
// an EtcdSnapshot that recorded the artifact; the agent just removes it.
//
// Idempotent: an object or file that is already gone is a success, not an
// error. The prune Job is retried on failure and re-run on a later reconcile,
// and neither should turn "already deleted" into a permanently failing policy.
func RunPrune(ctx context.Context) error {
	dest, err := loadDestination()
	if err != nil {
		return err
	}
	name := os.Getenv(envSnapshotName)
	if name == "" {
		return fmt.Errorf("%s is empty", envSnapshotName)
	}

	switch dest.kind {
	case "s3":
		return pruneS3(ctx, dest, name)
	case "pvc":
		return prunePVC(dest, name)
	default:
		return fmt.Errorf("unknown %s=%q (want s3|pvc)", envDestKind, dest.kind)
	}
}

func pruneS3(ctx context.Context, dest destination, name string) error {
	client, err := dest.s3Client(ctx)
	if err != nil {
		return err
	}
	key := dest.objectKey(name)
	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(dest.s3Bucket),
		Key:    aws.String(key),
	})
	if err != nil && !isS3NotFound(err) {
		return fmt.Errorf("delete s3://%s/%s: %w", dest.s3Bucket, key, err)
	}
	fmt.Printf("pruned %s\n", dest.uri(name))
	return nil
}

func prunePVC(dest destination, name string) error {
	path := dest.localPath(name)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete %s: %w", path, err)
	}
	fmt.Printf("pruned %s\n", dest.uri(name))
	return nil
}

// isS3NotFound reports whether the error is the backend saying the object is
// already gone. S3 itself answers DeleteObject on a missing key with 204, but
// S3-compatible backends are not uniform about it, so treat an explicit
// not-found as success rather than relying on that.
func isS3NotFound(err error) bool {
	var nf *s3types.NotFound
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nf) || errors.As(err, &nsk) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey", "404":
			return true
		}
	}
	return false
}
