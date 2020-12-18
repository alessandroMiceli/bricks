package objstore

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"time"

	"github.com/minio/minio-go/v6"
	"github.com/pace/bricks/maintenance/health/servicehealthcheck"
)

// HealthCheck checks the state of the object storage client. It must not be changed
// after it was registered as a health check.
type HealthCheck struct {
	state  servicehealthcheck.ConnectionState
	Client *minio.Client
}

var (
	healthCheckTimeFormat     = time.RFC3339
	healthCheckConcurrentSpan = 10 * time.Second
)

// HealthCheck checks if the object storage client is healthy. If the last result is outdated,
// object storage is checked for upload and download,
// otherwise returns the old result
func (h *HealthCheck) HealthCheck(ctx context.Context) servicehealthcheck.HealthCheckResult {
	if time.Since(h.state.LastChecked()) <= cfg.HealthCheckResultTTL {
		// the last health check is not outdated, an can be reused.
		return h.state.GetState()
	}

	checkTime := time.Now()
	expContent := []byte(checkTime.Format(healthCheckTimeFormat))
	expSize := int64(len(expContent))

	_, err := h.Client.PutObjectWithContext(
		ctx,
		cfg.HealthCheckBucketName,
		cfg.HealthCheckObjectName,
		bytes.NewReader(expContent),
		expSize,
		minio.PutObjectOptions{
			ContentType: "text/plain",
		},
	)
	if err != nil {
		h.state.SetErrorState(fmt.Errorf("failed to put object: %v", err))
		return h.state.GetState()
	}

	// Try download
	obj, err := h.Client.GetObjectWithContext(
		ctx,
		cfg.HealthCheckBucketName,
		cfg.HealthCheckObjectName,
		minio.GetObjectOptions{},
	)
	if err != nil {
		h.state.SetErrorState(fmt.Errorf("failed to get object: %v", err))
		return h.state.GetState()
	}
	defer obj.Close()

	// Assert expectations
	buf, err := ioutil.ReadAll(obj)
	if err != nil {
		h.state.SetErrorState(fmt.Errorf("failed to compare object: %v", err))
		return h.state.GetState()
	}

	if !bytes.Equal(buf, expContent) {
		if wasConcurrentHealthCheck(checkTime, string(buf)) {
			goto healthy
		}

		h.state.SetErrorState(fmt.Errorf("unexpected content: %q <-> %q", string(buf), string(expContent)))
		return h.state.GetState()
	}

healthy:
	// If uploading and downloading worked set the Health Check to healthy
	h.state.SetHealthy()
	return h.state.GetState()
}

// wasConcurrentHealthCheck checks if the time doesn't match in a certain
// time span concurrent request to the objstore may break the assumption
// that the value is the same, but in this case it would be acceptable.
// Assumption all instances are created equal and one providing evidence
// of a good write would be sufficient. See #244
func wasConcurrentHealthCheck(checkTime time.Time, observedValue string) bool {
	t, err := time.Parse(healthCheckTimeFormat, observedValue)
	if err == nil {
		allowedStart := checkTime.Add(-healthCheckConcurrentSpan)
		allowedEnd := checkTime.Add(healthCheckConcurrentSpan)

		// timestamp we got from the document is in allowed range
		// concider it healthy
		return t.After(allowedStart) && t.Before(allowedEnd)
	}

	return false
}
