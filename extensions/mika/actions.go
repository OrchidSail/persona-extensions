package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type runTaskArgs struct {
	Goal           string `json:"goal"`
	ConversationID string `json:"conversation_id,omitempty"`
	Conversation   string `json:"conversation,omitempty"`
	RequestID      string `json:"request_id,omitempty"`
	Device         string `json:"device,omitempty"`
}

func (in *runTaskArgs) normalize() error {
	goal, err := validTextInput(in.Goal, "goal", maxGoalChars)
	if err != nil {
		return err
	}
	in.Goal = goal
	convID := strings.TrimSpace(in.ConversationID)
	convAlias := strings.TrimSpace(in.Conversation)
	if convID != "" && convAlias != "" && convID != convAlias {
		return errors.New("conversation_id and conversation must match when both are provided")
	}
	if convID == "" {
		convID = convAlias
	}
	if convID != "" {
		if err := validConversationID(convID); err != nil {
			return err
		}
	}
	in.ConversationID = convID
	in.Conversation = ""
	reqID := strings.TrimSpace(in.RequestID)
	if reqID != "" {
		if err := validRequestUUID(reqID); err != nil {
			return err
		}
	}
	in.RequestID = reqID
	devSel, err := validDeviceSelector(in.Device)
	if err != nil {
		return err
	}
	in.Device = devSel
	return nil
}

type taskTextArgs struct {
	TaskID string `json:"task_id"`
	Text   string `json:"text"`
	Device string `json:"device,omitempty"`
}

type cancelTaskArgs struct {
	TaskID string `json:"task_id"`
	Device string `json:"device,omitempty"`
}

func isWriteTool(name string) bool {
	switch name {
	case "mika_run_task", "mika_answer", "mika_steer_task", "mika_cancel_task":
		return true
	default:
		return false
	}
}

func (a *app) describeAction(name string, raw json.RawMessage) (map[string]any, error) {
	st := a.snapshot()
	var summary string
	switch name {
	case "mika_run_task":
		var in runTaskArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.normalize(); err != nil {
			return nil, err
		}
		dev, err := st.resolveReadyDevice(in.Device)
		if err != nil {
			return nil, err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "[%s] Start a Mika task on the Android phone:\n\n", dev.confirmLabel())
		b.WriteString(in.Goal)
		if in.ConversationID != "" {
			fmt.Fprintf(&b, "\n\nConversation: %s", in.ConversationID)
		}
		if in.RequestID != "" {
			fmt.Fprintf(&b, "\nRequest ID: %s", in.RequestID)
		}
		b.WriteString("\n\nNote: payment, login and takeover steps remain on the phone.")
		summary = b.String()

	case "mika_answer":
		var in taskTextArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		id := strings.TrimSpace(in.TaskID)
		if err := validTaskID(id); err != nil {
			return nil, err
		}
		text, err := validTextInput(in.Text, "text", maxAnswerChars)
		if err != nil {
			return nil, err
		}
		dev, err := st.resolveReadyDevice(in.Device)
		if err != nil {
			return nil, err
		}
		summary = fmt.Sprintf("[%s] Answer Mika question for task %s:\n\n%s", dev.confirmLabel(), id, text)

	case "mika_steer_task":
		var in taskTextArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		id := strings.TrimSpace(in.TaskID)
		if err := validTaskID(id); err != nil {
			return nil, err
		}
		text, err := validTextInput(in.Text, "text", maxSteerChars)
		if err != nil {
			return nil, err
		}
		dev, err := st.resolveReadyDevice(in.Device)
		if err != nil {
			return nil, err
		}
		summary = fmt.Sprintf("[%s] Send follow-up instruction to running Mika task %s:\n\n%s", dev.confirmLabel(), id, text)

	case "mika_cancel_task":
		var in cancelTaskArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		id := strings.TrimSpace(in.TaskID)
		if err := validTaskID(id); err != nil {
			return nil, err
		}
		dev, err := st.resolveReadyDevice(in.Device)
		if err != nil {
			return nil, err
		}
		summary = fmt.Sprintf("[%s] Cancel Mika task %s", dev.confirmLabel(), id)

	default:
		return nil, fmt.Errorf("unknown action %q", name)
	}
	return map[string]any{
		"action":  map[string]any{"name": name, "visible": "owner"},
		"summary": summary,
	}, nil
}

type preparedWrite struct {
	dev       deviceEntry
	canonical any
	exec      func(context.Context) (string, error)
}

func (a *app) prepareWrite(st appState, name string, raw json.RawMessage) (preparedWrite, error) {
	switch name {
	case "mika_run_task":
		var in runTaskArgs
		if err := decode(raw, &in); err != nil {
			return preparedWrite{}, err
		}
		if err := in.normalize(); err != nil {
			return preparedWrite{}, err
		}
		dev, err := st.resolveReadyDevice(in.Device)
		if err != nil {
			return preparedWrite{}, err
		}
		canonical := runTaskArgs{
			Goal:           in.Goal,
			ConversationID: in.ConversationID,
			RequestID:      in.RequestID,
		}
		return preparedWrite{
			dev:       dev,
			canonical: canonical,
			exec: func(ctx context.Context) (string, error) {
				task, err := dev.client.startTask(ctx, startTaskRequest{
					Goal:         in.Goal,
					Conversation: in.ConversationID,
					RequestID:    in.RequestID,
				})
				a.recordDevice(dev.id, err)
				if err != nil {
					if in.RequestID != "" && !isDefiniteRefusal(err) {
						return "", fmt.Errorf("%w (check mika_task_status on %s with task_id=%s before retrying)", err, dev.label(), in.RequestID)
					}
					return "", err
				}
				var b strings.Builder
				fmt.Fprintf(&b, "Started Mika task %s on %s (status: %s)\n", task.TaskID, dev.label(), task.Status)
				if task.Conversation != "" {
					fmt.Fprintf(&b, "Conversation: %s\n", task.Conversation)
				}
				b.WriteString("Follow up with mika_wait_task (timeout_s=25 recommended). Payment, login or takeover confirmations must be handled directly on the Android phone.\n")
				return b.String(), nil
			},
		}, nil

	case "mika_answer":
		var in taskTextArgs
		if err := decode(raw, &in); err != nil {
			return preparedWrite{}, err
		}
		id := strings.TrimSpace(in.TaskID)
		if err := validTaskID(id); err != nil {
			return preparedWrite{}, err
		}
		text, err := validTextInput(in.Text, "text", maxAnswerChars)
		if err != nil {
			return preparedWrite{}, err
		}
		dev, err := st.resolveReadyDevice(in.Device)
		if err != nil {
			return preparedWrite{}, err
		}
		return preparedWrite{
			dev:       dev,
			canonical: taskTextArgs{TaskID: id, Text: text},
			exec: func(ctx context.Context) (string, error) {
				res, err := dev.client.answerTask(ctx, id, text)
				a.recordDevice(dev.id, err)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Answer submitted for Mika task %s on %s (accepted: %t). Continue with mika_wait_task.\n", res.TaskID, dev.label(), res.Accepted), nil
			},
		}, nil

	case "mika_steer_task":
		var in taskTextArgs
		if err := decode(raw, &in); err != nil {
			return preparedWrite{}, err
		}
		id := strings.TrimSpace(in.TaskID)
		if err := validTaskID(id); err != nil {
			return preparedWrite{}, err
		}
		text, err := validTextInput(in.Text, "text", maxSteerChars)
		if err != nil {
			return preparedWrite{}, err
		}
		dev, err := st.resolveReadyDevice(in.Device)
		if err != nil {
			return preparedWrite{}, err
		}
		return preparedWrite{
			dev:       dev,
			canonical: taskTextArgs{TaskID: id, Text: text},
			exec: func(ctx context.Context) (string, error) {
				res, err := dev.client.steerTask(ctx, id, text)
				a.recordDevice(dev.id, err)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Instruction queued for Mika task %s on %s (message_id: %s, accepted: %t). Check steers in mika_task_status to see whether it becomes joined.\n",
					res.TaskID, dev.label(), res.MessageID, res.Accepted), nil
			},
		}, nil

	case "mika_cancel_task":
		var in cancelTaskArgs
		if err := decode(raw, &in); err != nil {
			return preparedWrite{}, err
		}
		id := strings.TrimSpace(in.TaskID)
		if err := validTaskID(id); err != nil {
			return preparedWrite{}, err
		}
		dev, err := st.resolveReadyDevice(in.Device)
		if err != nil {
			return preparedWrite{}, err
		}
		return preparedWrite{
			dev:       dev,
			canonical: cancelTaskArgs{TaskID: id},
			exec: func(ctx context.Context) (string, error) {
				res, err := dev.client.cancelTask(ctx, id)
				a.recordDevice(dev.id, err)
				if err != nil {
					return "", err
				}
				if res.Accepted {
					return fmt.Sprintf("Cancellation requested for Mika task %s on %s (accepted: true). Poll mika_task_status until status is cancelled.\n", res.TaskID, dev.label()), nil
				}
				return fmt.Sprintf("Mika task %s on %s was already finished (accepted: false, status: %s).\n", res.TaskID, dev.label(), res.Status), nil
			},
		}, nil

	default:
		return preparedWrite{}, fmt.Errorf("unknown action %q", name)
	}
}

func (a *app) callWriteTool(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	st := a.snapshot()
	pw, err := a.prepareWrite(st, name, raw)
	if err != nil {
		return "", err
	}
	text, err := pw.exec(ctx)
	if err != nil {
		return "", err
	}
	if st.hub != nil {
		_ = st.hub.Invalidate(pageUI)
	}
	return text, nil
}

func (a *app) doAction(ctx context.Context, requestID, name string, raw json.RawMessage) (map[string]any, error) {
	st := a.snapshot()
	pw, err := a.prepareWrite(st, name, raw)
	if err != nil {
		return nil, err
	}
	keyData, err := json.Marshal(struct {
		DeviceID string `json:"device_id"`
		Action   string `json:"action"`
		Args     any    `json:"args"`
	}{DeviceID: pw.dev.id, Action: name, Args: pw.canonical})
	if err != nil {
		return nil, err
	}
	text, err := st.receipts.commit(receiptKey(pw.dev.id, requestID), string(keyData), func() (string, error) {
		return pw.exec(ctx)
	})
	if err != nil {
		return nil, err
	}
	if st.hub != nil {
		_ = st.hub.Invalidate(pageUI)
	}
	return map[string]any{"text": text}, nil
}
