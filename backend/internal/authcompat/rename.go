package authcompat

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

type aliasBucket struct {
	nextSuffix  int
	assignments map[string]string
}

// Renamer 保存进程内的每日命名游标和邮箱别名分配。插件重启后状态自然清空。
type Renamer struct {
	mu           sync.Mutex
	cursors      map[string]map[string]int
	emailAliases map[string]*aliasBucket
}

var processRenamer = NewRenamer()

func NewRenamer() *Renamer {
	return &Renamer{
		cursors:      map[string]map[string]int{},
		emailAliases: map[string]*aliasBucket{},
	}
}

// Rename 使用工具模块自己的进程级内存游标执行重命名。
func Rename(accounts []Account, now time.Time) []Account {
	return processRenamer.Rename(accounts, now)
}

// Rename 按 auths 工具的规则重命名账号，并默认应用其导入调度参数。
func (r *Renamer) Rename(accounts []Account, now time.Time) []Account {
	if len(accounts) == 0 {
		return accounts
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	location := time.FixedZone("CST", 8*60*60)
	current := now.In(location)
	datePart := current.Format("0102")
	dateKey := current.Format("20060102")
	for index := range accounts {
		account := &accounts[index]
		plan := accountPlan(*account)
		r.assignEmailAlias(account, plan, index)
		if account.PreserveName && strings.TrimSpace(account.Name) != "" {
			account.Name = strings.TrimSpace(account.Name)
		} else {
			cursorType := plan
			if cursorType == "" {
				cursorType = "default"
			}
			day := r.cursors[dateKey]
			if day == nil {
				day = map[string]int{}
				r.cursors[dateKey] = day
			}
			day[cursorType]++
			if plan == "" {
				account.Name = fmt.Sprintf("%s-%d", datePart, day[cursorType])
			} else {
				account.Name = fmt.Sprintf("%s-%s-%d", datePart, plan, day[cursorType])
			}
		}
		if account.Credentials == nil {
			account.Credentials = map[string]string{}
		}
		account.Credentials["account_name"] = account.Name
		account.Priority = 1
		account.MaxConcurrency = 15
	}
	return accounts
}

func accountPlan(account Account) string {
	credentials := account.Credentials
	payload := decodeJWTPayload(credentials["access_token"])
	auth := openAIAuth(payload)
	raw := firstString(credentials["plan_type"], credentials["chatgpt_plan_type"], auth["chatgpt_plan_type"])
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '-' || r == '_' || r == ' '
	})
	labels := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.ToLower(strings.TrimSpace(part))
		if part != "" {
			labels = append(labels, strings.ToUpper(part[:1])+part[1:])
		}
	}
	return strings.Join(labels, "")
}

func (r *Renamer) assignEmailAlias(account *Account, plan string, index int) {
	if account.Email == nil || !aliasPlanEligible(plan) {
		return
	}
	original := *account.Email
	baseEmail, local, domain, ok := emailAliasBase(original)
	if !ok {
		return
	}
	key := strings.ToLower(baseEmail)
	bucket := r.emailAliases[key]
	if bucket == nil {
		bucket = &aliasBucket{nextSuffix: 1, assignments: map[string]string{}}
		r.emailAliases[key] = bucket
	}
	identity := accountAliasIdentity(*account)
	if identity == "" {
		identity = fmt.Sprintf("name:%s:index:%d", strings.ToLower(account.Name), index)
	}
	assigned := bucket.assignments[identity]
	if assigned == "" {
		assigned = baseEmail
		if len(bucket.assignments) > 0 {
			assigned = fmt.Sprintf("%s+%d@%s", local, bucket.nextSuffix, domain)
			bucket.nextSuffix++
		}
		bucket.assignments[identity] = assigned
	}
	account.Email = &assigned
	if account.Credentials == nil {
		account.Credentials = map[string]string{}
	}
	account.Credentials["email"] = assigned
}

func aliasPlanEligible(plan string) bool {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "", "k12", "team":
		return true
	default:
		return false
	}
}

func emailAliasBase(email string) (string, string, string, bool) {
	trimmed := strings.TrimSpace(email)
	at := strings.LastIndex(trimmed, "@")
	if at <= 0 || at == len(trimmed)-1 {
		return "", "", "", false
	}
	local := trimmed[:at]
	domain := trimmed[at+1:]
	if plus := strings.LastIndex(local, "+"); plus > 0 && allDigits(local[plus+1:]) {
		local = local[:plus]
	}
	return local + "@" + domain, local, domain, true
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	_, err := strconv.Atoi(value)
	return err == nil
}

func accountAliasIdentity(account Account) string {
	credentials := account.Credentials
	accessPayload := decodeJWTPayload(credentials["access_token"])
	idPayload := decodeJWTPayload(credentials["id_token"])
	accessAuth := openAIAuth(accessPayload)
	idAuth := openAIAuth(idPayload)
	if accountID := firstString(credentials["chatgpt_account_id"], credentials["account_id"], accessAuth["chatgpt_account_id"], idAuth["chatgpt_account_id"]); accountID != "" {
		return "account:" + strings.ToLower(accountID)
	}
	if tokenID := firstString(accessPayload["jti"], idPayload["jti"]); tokenID != "" {
		return "token:" + strings.ToLower(tokenID)
	}
	if account.Name != "" {
		return "name:" + strings.ToLower(account.Name)
	}
	return ""
}
