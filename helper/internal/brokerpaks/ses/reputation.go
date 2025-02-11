package ses

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ses"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

const (
	snsMessageTypeHeader                   = "x-amz-sns-message-type"
	snsMessageTypeNotification             = "Notification"
	snsMessageTypeSubscriptionConfirmation = "SubscriptionConfirmation"
)

type SESClient interface {
	UpdateConfigurationSetSendingEnabled(context.Context, *ses.UpdateConfigurationSetSendingEnabledInput, ...func(*ses.Options)) (*ses.UpdateConfigurationSetSendingEnabledOutput, error)
}

type CloudWatchAlarm struct {
	AlarmName     string
	NewStateValue string
	Trigger       struct {
		Dimensions []struct {
			Name  string
			Value string
		}
	}
}

func (a *CloudWatchAlarm) Valid() map[string]string {
	verrs := make(map[string]string)
	prefix := "SES-BounceRate-Critical-Identity-"
	if !strings.HasPrefix(a.AlarmName, prefix) {
		verrs["AlarmName"] = fmt.Sprintf("expected alarm to have prefix %v, but name was %v", prefix, a.AlarmName)
	}
	if len(a.Trigger.Dimensions) == 0 {
		verrs["Trigger.Dimensions"] = fmt.Sprintf("expected one trigger dimension on the alarm, got 0")
		// return immediately to avoid index out of bounds panics
		return verrs
	}
	if l := len(a.Trigger.Dimensions); l > 1 {
		verrs["Trigger.Dimensions"] = fmt.Sprintf("expected only one trigger dimension on the alarm, got %v", l)
	}
	if name := a.Trigger.Dimensions[0].Name; name != "ConfigurationSetName" {
		verrs["Trigger.Dimensions[0].Name"] = fmt.Sprintf("expected alarm with name %v, got %v", "ConfigurationSetName", name)
	}
	return verrs
}

func UnmarshalMessage(body io.Reader) (SNSMessage, error) {
	var s SNSMessage
	b, err := io.ReadAll(body)
	if err != nil {
		return s, fmt.Errorf("reading SNS request body: %w", err)
	}
	if len(b) == 0 {
		return s, fmt.Errorf("SNS request body was 0 bytes")
	}
	err = json.Unmarshal(b, &s)
	if err != nil {
		return s, fmt.Errorf("unmarshalling SNS request body: %w", err)
	}
	return s, nil
}

func handleSubscriptionConfirmation(msg SNSMessage) {
	_, err := http.Get(msg.SubscribeURL)
	if err != nil {
		slog.Error("error confirming SNS subscription", "err", err)
	} else {
		slog.Info("confirmed subscription to SNS topic", "topic", msg.TopicArn)
	}
}

func handleNotification(ctx context.Context, msg SNSMessage, sesclient SESClient) {
	var a CloudWatchAlarm
	err := json.Unmarshal([]byte(msg.Message), &a)
	if err != nil {
		slog.Error("unmarshalling CloudWatch alarm from SNS message body", "err", err)
	}

	if errs := a.Valid(); len(errs) > 0 {
		slog.Error("error validating CloudWatch alarm. is the SNS subscription FilterPolicy allowing non-SES notifications?", "errs", errs)
	}

	cset := a.Trigger.Dimensions[0].Value
	slog.Info("pausing sending on SES identity via Configuration Set", "configuration-set", cset)
	_, err = sesclient.UpdateConfigurationSetSendingEnabled(ctx, &ses.UpdateConfigurationSetSendingEnabledInput{
		ConfigurationSetName: aws.String(cset),
		Enabled:              false,
	})
	if err != nil {
		slog.Error("error pausing sending on configuration set", "name", cset, "err", err)
	}
}

// HandleSNSRequest handles requests from the platform notifications SNS topic subscription.
func HandleSNSRequest(sesclient *ses.Client, snsclient *sns.Client) http.Handler {
	return http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			defer r.Body.Close() // todo, can return an error
			msg, err := UnmarshalMessage(r.Body)
			if err != nil {
				slog.Error("error processing CloudWatch alarm SNS request", "err", err)
				return
			}
			u, err := url.Parse(*snsclient.Options().BaseEndpoint)
			if err != nil {
				slog.Error("initialized SNS client had no base endpoint -- this should never happen")
			}

			if err = VerifySNSMessage(msg, u.Host); err != nil {
				slog.Error("failed to verify SNS message signature", "err", err)
				return
			}

			// once verified, switch on request type
			mtype := r.Header.Get(snsMessageTypeHeader)
			if mtype == "" {
				slog.Error("SNS message passed verification but type header was empty -- this should never happen")
				return
			}
			switch mtype {
			case snsMessageTypeSubscriptionConfirmation:
				handleSubscriptionConfirmation(msg)
			case snsMessageTypeNotification:
				handleNotification(r.Context(), msg, sesclient)
			default:
				// UnsubscribeConfirmation is a noop.
			}
		},
	)
}
