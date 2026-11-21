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
	return nil
}

type taskTextArgs struct {
	TaskID string `json:"task_id"`
	Text   string `json:"text"`
}

type cancelTaskArgs struct {
	TaskID string `json:"task_id"`
}

func describeAction(name string, raw json.RawMessage) (map[string]any, error) {
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
		var b strings.Builder
		b.WriteString("Start a Mika task on the Android phone:\n\n")
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
		summary = fmt.Sprintf("Answer Mika question for task %s:\n\n%s", id, text)

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
		summary = fmt.Sprintf("Send follow-up instruction to running Mika task %s:\n\n%s", id, text)

	case "mika_cancel_task":
		var in cancelTaskArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		id := strings.TrimSpace(in.TaskID)
		if err := validTaskID(id); err != nil {
			return nil, err
		}
		summary = fmt.Sprintf("Cancel Mika task %s", id)

	default:
		return nil, fmt.Errorf("unknown action %q", name)
	}
	return map[string]any{
		"action":  map[string]any{"name": name, "visible": "owner"},
		"summary": summary,
	}, nil
}

func (a *app) doAction(ctx context.Context, requestID, name string, raw json.RawMessage) (map[string]any, error) {
	st := a.snapshot()
	var (
		canonical any
		fn        func() (string, error)
	)
	switch name {
	case "mika_run_task":
		var in runTaskArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.normalize(); err != nil {
			return nil, err
		}
		canonical = in
		fn = func() (string, error) {
			task, err := st.client.startTask(ctx, startTaskRequest{
				Goal:         in.Goal,
				Conversation: in.ConversationID,
				RequestID:    in.RequestID,
			})
			a.record(err)
			if err != nil {
				if in.RequestID != "" && !isDefiniteRefusal(err) {
					return "", fmt.Errorf("%w (check mika_task_status with task_id=%s before retrying)", err, in.RequestID)
				}
				return "", err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Started Mika task %s (status: %s)\n", task.TaskID, task.Status)
			if task.Conversation != "" {
				fmt.Fprintf(&b, "Conversation: %s\n", task.Conversation)
			}
			b.WriteString("Follow up with mika_wait_task. Payment, login or takeover confirmations must be handled directly on the Android phone.\n")
			return b.String(), nil
		}

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
		canonical = taskTextArgs{TaskID: id, Text: text}
		fn = func() (string, error) {
			res, err := st.client.answerTask(ctx, id, text)
			a.record(err)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Answer submitted for Mika task %s (accepted: %t). Continue with mika_wait_task.\n", res.TaskID, res.Accepted), nil
		}

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
		canonical = taskTextArgs{TaskID: id, Text: text}
		fn = func() (string, error) {
			res, err := st.client.steerTask(ctx, id, text)
			a.record(err)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Instruction queued for Mika task %s (message_id: %s, accepted: %t). Check steers in mika_task_status to see whether it becomes joined.\n",
				res.TaskID, res.MessageID, res.Accepted), nil
		}

	case "mika_cancel_task":
		var in cancelTaskArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		id := strings.TrimSpace(in.TaskID)
		if err := validTaskID(id); err != nil {
			return nil, err
		}
		canonical = cancelTaskArgs{TaskID: id}
		fn = func() (string, error) {
			res, err := st.client.cancelTask(ctx, id)
			a.record(err)
			if err != nil {
				return "", err
			}
			if res.Accepted {
				return fmt.Sprintf("Cancellation requested for Mika task %s (accepted: true). Poll mika_task_status until status is cancelled.\n", res.TaskID), nil
			}
			return fmt.Sprintf("Mika task %s was already finished (accepted: false, status: %s).\n", res.TaskID, res.Status), nil
		}

	default:
		return nil, fmt.Errorf("unknown action %q", name)
	}

	keyData, err := json.Marshal(struct {
		Action string `json:"action"`
		Args   any    `json:"args"`
	}{Action: name, Args: canonical})
	if err != nil {
		return nil, err
	}
	text, err := st.receipts.commit(requestID, string(keyData), fn)
	if err != nil {
		return nil, err
	}
	if st.hub != nil {
		_ = st.hub.Invalidate(pageUI)
	}
	return map[string]any{"text": text}, nil
}
