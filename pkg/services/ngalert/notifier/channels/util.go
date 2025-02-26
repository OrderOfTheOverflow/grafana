package channels

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/grafana/alerting/alerting/notifier/channels"
	"github.com/prometheus/alertmanager/types"
	"github.com/prometheus/common/model"

	"github.com/grafana/grafana/pkg/services/ngalert/models"
)

var (
	// Provides current time. Can be overwritten in tests.
	timeNow = time.Now
)

type forEachImageFunc func(index int, image channels.Image) error

// getImage returns the image for the alert or an error. It returns a nil
// image if the alert does not have an image token or the image does not exist.
func getImage(ctx context.Context, l channels.Logger, imageStore channels.ImageStore, alert types.Alert) (*channels.Image, error) {
	token := getTokenFromAnnotations(alert.Annotations)
	if token == "" {
		return nil, nil
	}

	ctx, cancelFunc := context.WithTimeout(ctx, channels.ImageStoreTimeout)
	defer cancelFunc()

	img, err := imageStore.GetImage(ctx, token)
	if errors.Is(err, channels.ErrImageNotFound) || errors.Is(err, channels.ErrImagesUnavailable) {
		return nil, nil
	} else if err != nil {
		l.Warn("failed to get image with token", "token", token, "error", err)
		return nil, err
	} else {
		return img, nil
	}
}

// withStoredImages retrieves the image for each alert and then calls forEachFunc
// with the index of the alert and the retrieved image struct. If the alert does
// not have an image token, or the image does not exist then forEachFunc will not be
// called for that alert. If forEachFunc returns an error, withStoredImages will return
// the error and not iterate the remaining alerts. A forEachFunc can return ErrImagesDone
// to stop the iteration of remaining alerts if the intended image or maximum number of
// images have been found.
func withStoredImages(ctx context.Context, l channels.Logger, imageStore channels.ImageStore, forEachFunc forEachImageFunc, alerts ...*types.Alert) error {
	for index, alert := range alerts {
		logger := l.New("alert", alert.String())
		img, err := getImage(ctx, logger, imageStore, *alert)
		if err != nil {
			return err
		} else if img != nil {
			if err := forEachFunc(index, *img); err != nil {
				if errors.Is(err, channels.ErrImagesDone) {
					return nil
				}
				logger.Error("Failed to attach image to notification", "error", err)
				return err
			}
		}
	}
	return nil
}

// The path argument here comes from reading internal image storage, not user
// input, so we ignore the security check here.
//
//nolint:gosec
func openImage(path string) (io.ReadCloser, error) {
	fp := filepath.Clean(path)
	_, err := os.Stat(fp)
	if os.IsNotExist(err) || os.IsPermission(err) {
		return nil, channels.ErrImageNotFound
	}

	f, err := os.Open(fp)
	if err != nil {
		return nil, err
	}

	return f, nil
}

func getTokenFromAnnotations(annotations model.LabelSet) string {
	if value, ok := annotations[models.ImageTokenAnnotation]; ok {
		return string(value)
	}
	return ""
}

type receiverInitError struct {
	Reason string
	Err    error
	Cfg    channels.NotificationChannelConfig
}

func (e receiverInitError) Error() string {
	name := ""
	if e.Cfg.Name != "" {
		name = fmt.Sprintf("%q ", e.Cfg.Name)
	}

	s := fmt.Sprintf("failed to validate receiver %sof type %q: %s", name, e.Cfg.Type, e.Reason)
	if e.Err != nil {
		return fmt.Sprintf("%s: %s", s, e.Err.Error())
	}

	return s
}

func (e receiverInitError) Unwrap() error { return e.Err }

func getAlertStatusColor(status model.AlertStatus) string {
	if status == model.AlertFiring {
		return channels.ColorAlertFiring
	}
	return channels.ColorAlertResolved
}

type httpCfg struct {
	body     []byte
	user     string
	password string
}

// sendHTTPRequest sends an HTTP request.
// Stubbable by tests.
var sendHTTPRequest = func(ctx context.Context, url *url.URL, cfg httpCfg, logger channels.Logger) ([]byte, error) {
	var reader io.Reader
	if len(cfg.body) > 0 {
		reader = bytes.NewReader(cfg.body)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}
	if cfg.user != "" && cfg.password != "" {
		request.SetBasicAuth(cfg.user, cfg.password)
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "Grafana")
	netTransport := &http.Transport{
		TLSClientConfig: &tls.Config{
			Renegotiation: tls.RenegotiateFreelyAsClient,
		},
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	netClient := &http.Client{
		Timeout:   time.Second * 30,
		Transport: netTransport,
	}
	resp, err := netClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Warn("failed to close response body", "error", err)
		}
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode/100 != 2 {
		logger.Warn("HTTP request failed", "url", request.URL.String(), "statusCode", resp.Status, "body",
			string(respBody))
		return nil, fmt.Errorf("failed to send HTTP request - status code %d", resp.StatusCode)
	}

	logger.Debug("sending HTTP request succeeded", "url", request.URL.String(), "statusCode", resp.Status)
	return respBody, nil
}

func joinUrlPath(base, additionalPath string, logger channels.Logger) string {
	u, err := url.Parse(base)
	if err != nil {
		logger.Debug("failed to parse URL while joining URL", "url", base, "error", err.Error())
		return base
	}

	u.Path = path.Join(u.Path, additionalPath)

	return u.String()
}

// GetBoundary is used for overriding the behaviour for tests
// and set a boundary for multipart body. DO NOT set this outside tests.
var GetBoundary = func() string {
	return ""
}
