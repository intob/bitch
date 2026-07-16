package bridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/intob/bitch/internal/store"
)

// The /jobs Telegram surface (§6.1). Scheduling mechanics live in jobs.go;
// storage in internal/store/jobs.go.

const jobsHelp = `/jobs — list scheduled jobs
/jobs add <spec> [cap=<usd>] :: <prompt> — e.g. /jobs add 0 2 * * * :: pull main, run the tests, message me only if something broke
/jobs rm <id> — delete (with confirm)
/jobs run <id> — trigger now
/jobs pause <id> / /jobs resume <id>
/jobs tier <id> <0-3> — set the job's permission tier (with confirm)
Specs are cron (5 fields) or @every 4h / @daily. All times UTC.`

func (b *Bridge) cmdJobs(ctx context.Context, msg *tgbotapi.Message, key store.Key, arg string) {
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		b.listJobs(msg)
		return
	}
	sub := strings.ToLower(fields[0])
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(arg), fields[0]))
	switch sub {
	case "ls", "list":
		b.listJobs(msg)
	case "add", "new":
		b.addJob(msg, key, rest)
	case "rm", "del", "delete", "remove":
		b.confirmJobRm(msg, rest)
	case "run":
		b.runJobNow(ctx, msg, rest)
	case "pause":
		b.pauseJob(msg, rest, false)
	case "resume":
		b.pauseJob(msg, rest, true)
	case "tier":
		b.confirmJobTier(msg, rest)
	default:
		b.reply(msg.Chat.ID, jobsHelp)
	}
}

func (b *Bridge) listJobs(msg *tgbotapi.Message) {
	jobs, err := b.store.ListJobs(msg.Chat.ID, b.botID)
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	if len(jobs) == 0 {
		b.reply(msg.Chat.ID, "no scheduled jobs.\n\n"+jobsHelp)
		return
	}
	var sb strings.Builder
	sb.WriteString("⏰ scheduled jobs (times UTC):\n")
	for _, j := range jobs {
		state := "next " + j.NextRun.Format("Mon 02 Jan 15:04")
		if !j.Enabled {
			state = "paused"
		}
		fmt.Fprintf(&sb, "\n#%d  %s  ·  %s  ·  cap $%.2f\n%s\n",
			j.ID, j.Spec, state, j.CapUSD, truncateRunes(j.Prompt, 120))
		if j.LastStatus != "" {
			fmt.Fprintf(&sb, "last: %s (%s)\n", j.LastStatus, j.LastRun.Format("Mon 02 Jan 15:04"))
		}
	}
	b.reply(msg.Chat.ID, strings.TrimRight(sb.String(), "\n"))
}

// addJob parses `<spec> [cap=<usd>] :: <prompt>`. The `::` separator exists
// because a cron spec is a run of bare tokens and a prompt is free text — any
// positional split between them would be a guess.
func (b *Bridge) addJob(msg *tgbotapi.Message, key store.Key, arg string) {
	spec, prompt, capUSD, err := parseJobAdd(arg)
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error()+"\n\n"+jobsHelp)
		return
	}
	next, err := nextRun(spec, time.Now())
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error()+"\n\nspecs are cron (5 fields) or @every 4h / @daily.")
		return
	}
	// The job inherits the chat's dir and tier at creation time, copied into its
	// own session row: a later /tier in the chat must not silently re-arm a job
	// that was created at T1.
	sess, err := b.session(key)
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	job := &store.Job{
		ChatID:  msg.Chat.ID,
		BotID:   b.botID,
		Spec:    spec,
		Prompt:  prompt,
		WorkDir: sess.WorkDir,
		CapUSD:  capUSD,
		Enabled: true,
		NextRun: next,
	}
	id, err := b.store.AddJob(job)
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	if err := b.store.Upsert(&store.Session{
		Key:     store.JobKey(job.ChatID, job.BotID, id),
		WorkDir: sess.WorkDir,
		Tier:    sess.Tier,
	}); err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	b.reply(msg.Chat.ID, fmt.Sprintf(
		"⏰ job #%d created.\nschedule: %s (UTC)\nnext run: %s\ndir: %s\ntier: %s\ncap: $%.2f per run\n\n"+
			"the cap is checked before each run, not enforced mid-turn — the daily budget is the backstop.",
		id, spec, next.Format("Mon 02 Jan 15:04"), sess.WorkDir, tierNames[sess.Tier], capUSD))
}

// parseJobAdd splits `<spec> [cap=<usd>] :: <prompt>`. cap= may sit anywhere in
// the left half; whatever tokens remain are the spec.
func parseJobAdd(arg string) (spec, prompt string, capUSD float64, err error) {
	left, right, ok := strings.Cut(arg, "::")
	if !ok {
		return "", "", 0, fmt.Errorf("missing the :: separator between schedule and prompt")
	}
	prompt = strings.TrimSpace(right)
	if prompt == "" {
		return "", "", 0, fmt.Errorf("the prompt is empty")
	}
	capUSD = defaultCapUSD
	var specTokens []string
	for _, tok := range strings.Fields(left) {
		if v, found := strings.CutPrefix(tok, "cap="); found {
			c, cerr := strconv.ParseFloat(v, 64)
			if cerr != nil || c <= 0 {
				return "", "", 0, fmt.Errorf("cap must be a positive number of dollars, e.g. cap=5")
			}
			capUSD = c
			continue
		}
		specTokens = append(specTokens, tok)
	}
	spec = strings.Join(specTokens, " ")
	if spec == "" {
		return "", "", 0, fmt.Errorf("the schedule is empty")
	}
	return spec, prompt, capUSD, nil
}

// ownedJob resolves an id argument to a job this chat owns. Scoping the lookup
// to the chat keeps job ids from being a cross-chat handle when this goes
// multi-agent (§11).
func (b *Bridge) ownedJob(chatID int64, arg string) (*store.Job, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(arg), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("expected a job id — see /jobs")
	}
	job, err := b.store.GetJob(id)
	if err != nil {
		return nil, err
	}
	if job == nil || job.ChatID != chatID || job.BotID != b.botID {
		return nil, fmt.Errorf("no job #%d here", id)
	}
	return job, nil
}

func (b *Bridge) runJobNow(ctx context.Context, msg *tgbotapi.Message, arg string) {
	job, err := b.ownedJob(msg.Chat.ID, arg)
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	key := store.JobKey(job.ChatID, job.BotID, job.ID)
	runCtx, cancel := context.WithCancel(ctx)
	if !b.claimIdle(key, cancel) {
		cancel()
		b.reply(msg.Chat.ID, fmt.Sprintf("⏳ job #%d is already running.", job.ID))
		return
	}
	// A manual trigger doesn't disturb the schedule: next_run stays as it was.
	go b.runJobTurn(runCtx, key, *job)
}

func (b *Bridge) pauseJob(msg *tgbotapi.Message, arg string, resume bool) {
	job, err := b.ownedJob(msg.Chat.ID, arg)
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	next := job.NextRun
	if resume {
		// Recompute from now: a job paused past its next occurrence would
		// otherwise fire the instant it's resumed.
		if next, err = nextRun(job.Spec, time.Now()); err != nil {
			b.reply(msg.Chat.ID, "⚠️ "+err.Error())
			return
		}
	}
	if err := b.store.SetJobEnabled(job.ID, resume, next); err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	if resume {
		b.reply(msg.Chat.ID, fmt.Sprintf("▶️ job #%d resumed — next run %s UTC.",
			job.ID, next.Format("Mon 02 Jan 15:04")))
		return
	}
	b.reply(msg.Chat.ID, fmt.Sprintf("⏸️ job #%d paused.", job.ID))
}

func (b *Bridge) confirmJobRm(msg *tgbotapi.Message, arg string) {
	job, err := b.ownedJob(msg.Chat.ID, arg)
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	confirm := tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf(
		"Delete job <b>#%d</b> (%s)?\n<code>%s</code>",
		job.ID, tgEscape(job.Spec), tgEscape(truncateRunes(job.Prompt, 120))))
	confirm.ParseMode = tgbotapi.ModeHTML
	confirm.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Delete", fmt.Sprintf("jobrm:%d", job.ID)),
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "jobrmx"),
		),
	)
	if _, err := b.bot.Send(confirm); err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
	}
}

func (b *Bridge) confirmJobRmCallback(cb *tgbotapi.CallbackQuery, data string) {
	if cb.Message == nil {
		b.sender.AnswerCallback(cb.ID, "error")
		return
	}
	job, err := b.ownedJob(cb.Message.Chat.ID, strings.TrimPrefix(data, "jobrm:"))
	if err != nil {
		b.sender.AnswerCallback(cb.ID, "error")
		b.sender.ResolveApproval(cb.Message.Chat.ID, cb.Message.MessageID, "⚠️ "+tgEscape(err.Error()))
		return
	}
	if _, err := b.store.DeleteJob(job.ID); err != nil {
		b.sender.AnswerCallback(cb.ID, "error")
		b.sender.ResolveApproval(cb.Message.Chat.ID, cb.Message.MessageID, "⚠️ "+tgEscape(err.Error()))
		return
	}
	b.sender.AnswerCallback(cb.ID, "deleted")
	b.sender.ResolveApproval(cb.Message.Chat.ID, cb.Message.MessageID,
		fmt.Sprintf("🗑️ deleted job #%d", job.ID))
}

// confirmJobTier is /tier for a job's session — same confirm-button flow,
// because it's the same security-relevant change; the job just has no chat of
// its own to type it into.
func (b *Bridge) confirmJobTier(msg *tgbotapi.Message, arg string) {
	idArg, tierArg, _ := strings.Cut(strings.TrimSpace(arg), " ")
	job, err := b.ownedJob(msg.Chat.ID, idArg)
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	n, err := strconv.Atoi(strings.TrimSpace(tierArg))
	if err != nil || n < 0 || n > 3 {
		b.reply(msg.Chat.ID, "usage: /jobs tier <id> <0-3>")
		return
	}
	confirm := tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf(
		"Run job <b>#%d</b> at <b>%s</b>?", job.ID, tgEscape(tierNames[n])))
	confirm.ParseMode = tgbotapi.ModeHTML
	confirm.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Confirm", fmt.Sprintf("jobtier:%d:%d", job.ID, n)),
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "jobtierx"),
		),
	)
	if _, err := b.bot.Send(confirm); err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
	}
}

func (b *Bridge) confirmJobTierCallback(cb *tgbotapi.CallbackQuery, data string) {
	if cb.Message == nil {
		b.sender.AnswerCallback(cb.ID, "error")
		return
	}
	idArg, tierArg, ok := strings.Cut(strings.TrimPrefix(data, "jobtier:"), ":")
	if !ok {
		b.sender.AnswerCallback(cb.ID, "error")
		return
	}
	n, err := strconv.Atoi(tierArg)
	if err != nil || n < 0 || n > 3 {
		b.sender.AnswerCallback(cb.ID, "error")
		return
	}
	job, err := b.ownedJob(cb.Message.Chat.ID, idArg)
	if err != nil {
		b.sender.AnswerCallback(cb.ID, "error")
		b.sender.ResolveApproval(cb.Message.Chat.ID, cb.Message.MessageID, "⚠️ "+tgEscape(err.Error()))
		return
	}
	if err := b.ensureJobSession(*job); err != nil {
		b.sender.AnswerCallback(cb.ID, "error")
		return
	}
	key := store.JobKey(job.ChatID, job.BotID, job.ID)
	sess, err := b.store.Get(key)
	if err != nil || sess == nil {
		b.sender.AnswerCallback(cb.ID, "error")
		return
	}
	sess.Tier = n
	if err := b.store.Upsert(sess); err != nil {
		b.sender.AnswerCallback(cb.ID, "error")
		return
	}
	b.sender.AnswerCallback(cb.ID, "tier set")
	b.sender.ResolveApproval(cb.Message.Chat.ID, cb.Message.MessageID,
		fmt.Sprintf("✅ job #%d now at %s", job.ID, tgEscape(tierNames[n])))
}
