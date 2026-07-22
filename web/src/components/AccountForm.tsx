import { useCallback, useEffect, useMemo, useState } from 'react';
import { cssVar } from '@devilgenius/airgate-theme';
import type {
  AccountFormProps,
  PluginBatchAccountInput,
  PluginOAuthBatchExchangeResult,
  PluginOAuthBridge,
  PluginOAuthExchangeResult,
} from '@devilgenius/airgate-theme/plugin';

type BatchExchangeResult = PluginOAuthBatchExchangeResult;
type BatchAccountInput = PluginBatchAccountInput;

// Session 导入能力暂未沉淀进 SDK，先在插件本地扩展 bridge 类型。
// Core v0.2.6+ 注入对应的实现；老版本 Core 下两个字段为 undefined，UI 自动隐藏 pill。
type OAuthBridgeWithSession = PluginOAuthBridge & {
  importSession?: (session: string) => Promise<PluginOAuthExchangeResult>;
  batchImportSession?: (sessions: string[]) => Promise<PluginOAuthBatchExchangeResult[]>;
};

/** 订阅计划显示名称和颜色映射 */
const planDisplayMap: Record<string, { label: string; color: string; bg: string }> = {
  free: { label: 'Free', color: '#6b7280', bg: '#f3f4f6' },
  plus: { label: 'Plus', color: '#059669', bg: '#d1fae5' },
  pro: { label: 'Pro', color: '#7c3aed', bg: '#ede9fe' },
  team: { label: 'Team', color: '#2563eb', bg: '#dbeafe' },
};

const inputStyle: React.CSSProperties = {
  display: 'block',
  width: '100%',
  borderRadius: cssVar('radiusMd'),
  borderWidth: '1px',
  borderStyle: 'solid',
  borderColor: cssVar('border'),
  backgroundColor: cssVar('fieldBackground'),
  padding: '0.5rem 0.75rem',
  fontSize: '0.875rem',
  color: cssVar('text'),
  outline: 'none',
  transition: 'border-color 0.2s, box-shadow 0.2s',
};

const labelStyle: React.CSSProperties = {
  display: 'block',
  fontSize: '0.75rem',
  fontWeight: 500,
  color: cssVar('textSecondary'),
  textTransform: 'uppercase',
  letterSpacing: '0.05em',
  marginBottom: '0.375rem',
};

const cardStyle: React.CSSProperties = {
  borderWidth: '1px',
  borderStyle: 'solid',
  borderColor: cssVar('border'),
  borderRadius: cssVar('radiusLg'),
  padding: '0.5rem 0.75rem',
  cursor: 'pointer',
  lineHeight: 1.2,
  transition: 'border-color 0.2s, background-color 0.2s',
};

const cardActiveStyle: React.CSSProperties = {
  borderWidth: '1px',
  borderStyle: 'solid',
  borderColor: cssVar('primary'),
  borderRadius: cssVar('radiusLg'),
  padding: '0.5rem 0.75rem',
  cursor: 'pointer',
  lineHeight: 1.2,
  transition: 'border-color 0.2s, background-color 0.2s',
  backgroundColor: cssVar('primarySubtle'),
};

const accountTypeTitleStyle: React.CSSProperties = {
  fontSize: '0.8125rem',
  fontWeight: 500,
  color: cssVar('text'),
  lineHeight: 1.2,
};

const accountTypeDescStyle: React.CSSProperties = {
  fontSize: '0.6875rem',
  color: cssVar('textTertiary'),
  lineHeight: 1.2,
  marginTop: '0.125rem',
};

const descStyle: React.CSSProperties = {
  fontSize: '0.75rem',
  color: cssVar('textTertiary'),
  marginTop: '0.25rem',
};

const pillStyle: React.CSSProperties = {
  display: 'inline-block',
  padding: '0.25rem 0.75rem',
  borderRadius: '9999px',
  fontSize: '0.75rem',
  cursor: 'pointer',
  transition: 'all 0.15s',
  borderWidth: '1px',
  borderStyle: 'solid',
  borderColor: cssVar('border'),
  color: cssVar('textSecondary'),
  backgroundColor: 'transparent',
};

const pillActiveStyle: React.CSSProperties = {
  ...pillStyle,
  borderColor: cssVar('primary'),
  color: cssVar('primary'),
  backgroundColor: cssVar('primarySubtle'),
};

type AccountType = 'apikey' | 'oauth';

function isAgentIdentityCredentials(credentials: Record<string, string>): boolean {
  const mode = (credentials.auth_mode || credentials.authMode || '')
    .trim()
    .toLowerCase()
    .replace(/_/g, '');
  return mode === 'agentidentity' || Boolean(
    credentials.agent_private_key ||
      credentials.agentPrivateKey ||
      credentials.agent_runtime_id ||
      credentials.agentRuntimeId,
  );
}

/** parseSessionLines —— 批量 session 输入解析：支持两种格式混合
 *  1) 每行一个完整的 /api/auth/session JSON（必须是单行 JSON）
 *  2) 每行一个裸 sessionToken（JWE 串）
 *  # 开头视为注释行。整个 textarea 也可以贴一个多行 JSON——会被合并为一项。
 */
function parseSessionLines(text: string): string[] {
  const trimmed = text.trim();
  if (trimmed.startsWith('{') && trimmed.endsWith('}')) {
    // 用户粘了一个跨行的 JSON——视作单条
    return [trimmed];
  }
  return trimmed
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line.length > 0 && !line.startsWith('#'));
}

/** OAuth 导入模式：浏览器授权 / 单个 Refresh Token / 批量 Refresh Token */
type OAuthMode = 'browser' | 'refresh_single' | 'refresh_batch' | 'session_single' | 'session_batch';
type RefreshTokenImportType = 'codex' | 'mobile';

const mobileRefreshTokenClientID = 'app_LlGpXReQgckcGGUo2JrYvtJK';

function detectType(credentials: Record<string, string>): AccountType | '' {
  if (credentials.api_key) return 'apikey';
  if (credentials.access_token || isAgentIdentityCredentials(credentials)) return 'oauth';
  return '';
}

/** 从 JWT access_token 中解析订阅信息（不验签） */
function parseJWTSubscription(token: string): { planType: string; subscriptionUntil: string } {
  try {
    const parts = token.split('.');
    if (parts.length !== 3) return { planType: '', subscriptionUntil: '' };
    const payload = JSON.parse(atob(parts[1].replace(/-/g, '+').replace(/_/g, '/')));
    const auth = payload['https://api.openai.com/auth'] || {};
    return {
      planType: auth.chatgpt_plan_type || '',
      subscriptionUntil: auth.chatgpt_subscription_active_until
        ? String(auth.chatgpt_subscription_active_until)
        : '',
    };
  } catch {
    return { planType: '', subscriptionUntil: '' };
  }
}

function parseRefreshTokenLines(text: string): string[] {
  return text
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line.length > 0 && !line.startsWith('#'));
}

/** 密钥类输入框：值默认被覆盖层遮住，聚焦输入框后覆盖层消失，可直接选中复制 */
function SecretInput({
  value,
  onChange,
  placeholder,
  name,
}: {
  value: string;
  onChange: (e: React.ChangeEvent<HTMLInputElement>) => void;
  placeholder?: string;
  name?: string;
}) {
  const [focused, setFocused] = useState(false);
  const masked = Boolean(value) && !focused;
  return (
    <div style={{ position: 'relative' }}>
      <input
        name={name}
        type="text"
        autoComplete="off"
        style={inputStyle}
        placeholder={placeholder}
        value={value}
        onChange={onChange}
        onFocus={() => setFocused(true)}
        onBlur={() => setFocused(false)}
      />
      <div
        aria-hidden="true"
        style={{
          position: 'absolute',
          inset: '1px',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          borderRadius: cssVar('radiusMd'),
          backgroundColor: cssVar('fieldBackground'),
          color: cssVar('textTertiary'),
          fontSize: '0.75rem',
          letterSpacing: '0.05em',
          pointerEvents: 'none',
          opacity: masked ? 1 : 0,
          transition: 'opacity 0.15s',
        }}
      >
        聚焦查看凭证
      </div>
    </div>
  );
}

function StatusMessage({ status }: { status: { type: 'info' | 'success' | 'error'; text: string } | null }) {
  if (!status) return null;
  return (
    <div
      style={{
        fontSize: '0.75rem',
        color:
          status.type === 'error'
            ? cssVar('danger')
            : status.type === 'success'
              ? cssVar('success')
              : cssVar('textSecondary'),
      }}
    >
      {status.text}
    </div>
  );
}

export function AccountForm({
  credentials,
  onChange,
  mode,
  accountType: propType,
  onAccountTypeChange,
  onSuggestedName,
  onBatchModeChange,
  onBatchImport,
  oauth,
}: AccountFormProps) {
  const [localType, setLocalType] = useState<AccountType | ''>(
    (propType as AccountType) || (mode === 'edit' ? detectType(credentials) : ''),
  );
  const [oauthMode, setOauthMode] = useState<OAuthMode>('browser');
  const [authorizeURL, setAuthorizeURL] = useState('');
  const [callbackURL, setCallbackURL] = useState('');
  const [refreshTokenInput, setRefreshTokenInput] = useState('');
  const [refreshTokenImportType, setRefreshTokenImportType] = useState<RefreshTokenImportType>('codex');
  const [sessionInput, setSessionInput] = useState('');
  const [sessionBatchText, setSessionBatchText] = useState('');
  const [batchText, setBatchText] = useState('');
  const [batchPhase, setBatchPhase] = useState<'input' | 'running' | 'result'>('input');
  const [batchResults, setBatchResults] = useState<BatchExchangeResult[]>([]);
  const [batchImportedCount, setBatchImportedCount] = useState(0);
  const [oauthLoading, setOAuthLoading] = useState(false);
  const [oauthStatus, setOAuthStatus] = useState<{ type: 'info' | 'success' | 'error'; text: string } | null>(null);
  const [credentialsExpanded, setCredentialsExpanded] = useState(false);
  const accountType = (propType as AccountType | undefined) ?? localType;
  const oauthExt = oauth as OAuthBridgeWithSession | undefined;
  const agentIdentity = isAgentIdentityCredentials(credentials);

  // 账号凭证区域徽章：标明账号当前持有的凭证类型
  const credentialBadges = useMemo(() => {
    const badges: string[] = [];
    if (agentIdentity) badges.push('Agent Identity');
    if (credentials.access_token) badges.push('Access Token');
    if (credentials.refresh_token) badges.push('Refresh Token');
    if (credentials.session_token) badges.push('Session');
    if (credentials.id_token) badges.push('ID Token');
    if (credentials.api_key) badges.push('API Key');
    return badges;
  }, [agentIdentity, credentials.access_token, credentials.refresh_token, credentials.session_token, credentials.id_token, credentials.api_key]);

  // 进入/退出批量模式时通知外层隐藏"下一步/创建"按钮
  const isBatchActive =
    mode === 'create' &&
    accountType === 'oauth' &&
    (oauthMode === 'refresh_batch' || oauthMode === 'session_batch');
  useEffect(() => {
    onBatchModeChange?.(isBatchActive);
  }, [isBatchActive, onBatchModeChange]);

  // 从 credentials 中读取订阅信息，没有则从 access_token JWT 解析
  const jwtInfo = useMemo(
    () => (!credentials.plan_type && credentials.access_token)
      ? parseJWTSubscription(credentials.access_token)
      : null,
    [credentials.access_token, credentials.plan_type],
  );
  const planType = credentials.plan_type || jwtInfo?.planType || '';
  const subscriptionUntil = credentials.subscription_active_until || jwtInfo?.subscriptionUntil || '';
  const batchRefreshTokens = useMemo(() => parseRefreshTokenLines(batchText), [batchText]);
  const batchSessionItems = useMemo(() => parseSessionLines(sessionBatchText), [sessionBatchText]);

  const updateField = useCallback(
    (key: string, value: string) => {
      onChange({ ...credentials, [key]: value });
    },
    [credentials, onChange],
  );

  const handleTypeChange = useCallback(
    (type: AccountType) => {
      setLocalType(type);
      onAccountTypeChange?.(type);
      setAuthorizeURL('');
      setCallbackURL('');
      setOAuthStatus(null);
      setOauthMode('browser');
      setRefreshTokenInput('');
      setRefreshTokenImportType('codex');
      setSessionInput('');
      setSessionBatchText('');
      setBatchText('');
      setBatchPhase('input');
      setBatchResults([]);
      setBatchImportedCount(0);
      const baseUrl = credentials.base_url || '';
      if (type === 'apikey') {
        onChange({ api_key: '', base_url: baseUrl, provider: '' });
      } else {
        const nextCredentials: Record<string, string> = {
          access_token: '',
          refresh_token: '',
          session_token: '',
          chatgpt_account_id: '',
          base_url: baseUrl,
          provider: '',
        };
        // Switching modes should not discard an imported Agent Identity key.
        for (const key of [
          'auth_mode',
          'agent_private_key',
          'agent_runtime_id',
          'task_id',
          'id_token',
          'chatgpt_account_id',
          'chatgpt_user_id',
          'chatgpt_account_is_fedramp',
          'account_id',
          'workspace_id',
          'email',
          'client_id',
          'plan_type',
          'subscription_active_until',
        ]) {
          if (credentials[key] !== undefined) nextCredentials[key] = credentials[key];
        }
        onChange(nextCredentials);
      }
    },
    [credentials, onChange, onAccountTypeChange],
  );

  const startOAuth = useCallback(async () => {
    if (!oauth) return;
    setOAuthLoading(true);
    setOAuthStatus({ type: 'info', text: '正在生成授权链接...' });
    try {
      const result = await oauth.start();
      setAuthorizeURL(result.authorizeURL);
      setCallbackURL('');
      setOAuthStatus({ type: 'success', text: '授权链接已生成，请复制到浏览器完成授权。' });
    } catch (error) {
      setOAuthStatus({
        type: 'error',
        text: error instanceof Error ? error.message : '生成授权链接失败',
      });
    } finally {
      setOAuthLoading(false);
    }
  }, [oauth]);

  const submitOAuthCallback = useCallback(async () => {
    if (!oauth || !callbackURL.trim()) return;
    setOAuthLoading(true);
    setOAuthStatus({ type: 'info', text: '正在完成授权交换...' });
    try {
      const result = await oauth.exchange(callbackURL.trim());
      onAccountTypeChange?.(result.accountType || 'oauth');
      onChange({ ...credentials, ...result.credentials });
      if (result.accountName) {
        onSuggestedName?.(result.accountName);
      }
      setOAuthStatus({ type: 'success', text: '授权成功，凭证已自动填充。' });
    } catch (error) {
      setOAuthStatus({
        type: 'error',
        text: error instanceof Error ? error.message : '授权交换失败',
      });
    } finally {
      setOAuthLoading(false);
    }
  }, [oauth, callbackURL, onAccountTypeChange, onChange, credentials, onSuggestedName]);

  const submitRefreshTokenImport = useCallback(async () => {
    if (!oauth?.importRefresh || !refreshTokenInput.trim()) return;
    const clientId = refreshTokenImportType === 'mobile' ? mobileRefreshTokenClientID : undefined;
    setOAuthLoading(true);
    setOAuthStatus({ type: 'info', text: '正在使用 Refresh Token 换取凭证...' });
    try {
      const result = await oauth.importRefresh(refreshTokenInput.trim(), clientId);
      onAccountTypeChange?.(result.accountType || 'oauth');
      onChange({ ...credentials, ...result.credentials });
      if (result.accountName) {
        onSuggestedName?.(result.accountName);
      }
      setOAuthStatus({ type: 'success', text: '导入成功，凭证已自动填充。' });
      setRefreshTokenInput('');
    } catch (error) {
      setOAuthStatus({
        type: 'error',
        text: error instanceof Error ? error.message : 'Refresh Token 导入失败',
      });
    } finally {
      setOAuthLoading(false);
    }
  }, [oauth, refreshTokenInput, refreshTokenImportType, onAccountTypeChange, onChange, credentials, onSuggestedName]);

  const submitBatchRefreshImport = useCallback(async () => {
    if (!oauth?.batchImportRefresh || !onBatchImport) {
      setOAuthStatus({ type: 'error', text: '当前环境不支持批量导入' });
      return;
    }
    const tokens = batchRefreshTokens;
    if (tokens.length === 0) {
      setOAuthStatus({ type: 'error', text: '请至少粘贴一个 Refresh Token（每行一个）' });
      return;
    }
    const clientId = refreshTokenImportType === 'mobile' ? mobileRefreshTokenClientID : undefined;
    setBatchPhase('running');
    setOAuthStatus({ type: 'info', text: `正在批量换取 ${tokens.length} 个 Token...` });
    try {
      const results = await oauth.batchImportRefresh(tokens, clientId);
      setBatchResults(results);
      const successItems = results.filter((r) => r.status === 'ok' && r.credentials);
      if (successItems.length > 0) {
        const accounts: BatchAccountInput[] = successItems.map((r) => ({
          name: r.accountName || r.credentials.email || 'OpenAI OAuth',
          type: r.accountType || 'oauth',
          credentials: r.credentials,
        }));
        const importResp = await onBatchImport(accounts);
        setBatchImportedCount(importResp.imported);
      }
      setBatchPhase('result');
      setOAuthStatus(null);
    } catch (err) {
      setBatchPhase('input');
      setOAuthStatus({ type: 'error', text: err instanceof Error ? err.message : '批量导入失败' });
    }
  }, [batchRefreshTokens, refreshTokenImportType, oauth, onBatchImport]);

  const submitSessionImport = useCallback(async () => {
    if (!oauthExt?.importSession || !sessionInput.trim()) return;
    setOAuthLoading(true);
    setOAuthStatus({ type: 'info', text: '正在解析 session 并导入...' });
    try {
      const result = await oauthExt.importSession(sessionInput.trim());
      onAccountTypeChange?.(result.accountType || 'oauth');
      onChange({ ...credentials, ...result.credentials });
      if (result.accountName) {
        onSuggestedName?.(result.accountName);
      }
      setOAuthStatus({ type: 'success', text: '导入成功，凭证已自动填充。' });
      setSessionInput('');
    } catch (error) {
      setOAuthStatus({
        type: 'error',
        text: error instanceof Error ? error.message : 'Session 导入失败',
      });
    } finally {
      setOAuthLoading(false);
    }
  }, [oauthExt, sessionInput, onAccountTypeChange, onChange, credentials, onSuggestedName]);

  const submitBatchSessionImport = useCallback(async () => {
    if (!oauthExt?.batchImportSession || !onBatchImport) {
      setOAuthStatus({ type: 'error', text: '当前环境不支持批量导入' });
      return;
    }
    const sessions = batchSessionItems;
    if (sessions.length === 0) {
      setOAuthStatus({ type: 'error', text: '请至少粘贴一个 session JSON 或 sessionToken（每行一个）' });
      return;
    }
    setBatchPhase('running');
    setOAuthStatus({ type: 'info', text: `正在批量解析 ${sessions.length} 个 session...` });
    try {
      const results = await oauthExt.batchImportSession(sessions);
      setBatchResults(results);
      const successItems = results.filter((r) => r.status === 'ok' && r.credentials);
      if (successItems.length > 0) {
        const accounts: BatchAccountInput[] = successItems.map((r) => ({
          name: r.accountName || r.credentials.email || 'OpenAI Session',
          type: r.accountType || 'oauth',
          credentials: r.credentials,
        }));
        const importResp = await onBatchImport(accounts);
        setBatchImportedCount(importResp.imported);
      }
      setBatchPhase('result');
      setOAuthStatus(null);
    } catch (err) {
      setBatchPhase('input');
      setOAuthStatus({ type: 'error', text: err instanceof Error ? err.message : '批量导入失败' });
    }
  }, [batchSessionItems, oauthExt, onBatchImport]);

  const resetBatch = useCallback(() => {
    setBatchText('');
    setSessionBatchText('');
    setRefreshTokenImportType('codex');
    setBatchPhase('input');
    setBatchResults([]);
    setBatchImportedCount(0);
    setOAuthStatus(null);
  }, []);

  const copyAuthorizeURL = useCallback(async () => {
    if (!authorizeURL) return;

    // 尝试 1: Clipboard API（HTTPS 下可用）
    if (navigator.clipboard?.writeText) {
      try {
        await navigator.clipboard.writeText(authorizeURL);
        setOAuthStatus({ type: 'success', text: '授权链接已复制到剪贴板。' });
        return;
      } catch { /* 继续回退 */ }
    }

    // 尝试 2: execCommand（兼容旧浏览器和部分 HTTP 场景）
    try {
      const textarea = document.createElement('textarea');
      textarea.value = authorizeURL;
      textarea.setAttribute('readonly', '');
      textarea.style.position = 'fixed';
      textarea.style.left = '-9999px';
      document.body.appendChild(textarea);
      textarea.focus();
      textarea.select();
      const ok = document.execCommand('copy');
      document.body.removeChild(textarea);
      if (ok) {
        setOAuthStatus({ type: 'success', text: '授权链接已复制到剪贴板。' });
        return;
      }
    } catch { /* 继续回退 */ }

    // 尝试 3: 选中授权链接文本，提示用户手动 Ctrl+C
    const el = document.querySelector<HTMLTextAreaElement>('textarea[readonly]');
    if (el) {
      el.focus();
      el.select();
    }
    setOAuthStatus({ type: 'error', text: '自动复制不可用，请手动选中上方链接并按 Ctrl+C 复制。' });
  }, [authorizeURL]);

  const primaryBtnStyle = (disabled: boolean): React.CSSProperties => ({
    ...inputStyle,
    cursor: disabled ? 'not-allowed' : 'pointer',
    backgroundColor: cssVar('primary'),
    color: cssVar('primaryForeground'),
    borderColor: 'transparent',
    fontWeight: 500,
    width: 'auto',
    opacity: disabled ? 0.6 : 1,
  });

  const ghostBtnStyle = (disabled: boolean): React.CSSProperties => ({
    ...inputStyle,
    cursor: disabled ? 'not-allowed' : 'pointer',
    backgroundColor: cssVar('fieldBackground'),
    color: cssVar('text'),
    width: 'auto',
    opacity: disabled ? 0.6 : 1,
  });

  const outlineBtnStyle = (disabled: boolean): React.CSSProperties => ({
    ...inputStyle,
    cursor: disabled ? 'not-allowed' : 'pointer',
    backgroundColor: cssVar('fieldBackground'),
    color: cssVar('primary'),
    borderColor: cssVar('primary'),
    width: 'auto',
    opacity: disabled ? 0.6 : 1,
  });

  const refreshTokenTypeField = (
    <div style={{ marginBottom: '0.75rem' }}>
      <label style={labelStyle}>Refresh Token 类型</label>
      <select
        style={inputStyle}
        value={refreshTokenImportType}
        onChange={(e) => setRefreshTokenImportType(e.target.value as RefreshTokenImportType)}
      >
        <option value="codex">普通 RT</option>
        <option value="mobile">Mobile RT</option>
      </select>
      <div style={descStyle}>普通 RT 使用后端默认 client_id；Mobile RT 使用 Sora Mobile client_id。</div>
    </div>
  );

  // Agent Identity 凭证。普通 OAuth 可留空；Agent Identity 不需要 access_token。
  const agentIdentityBlock = (
    <div style={{
      borderWidth: '1px',
      borderStyle: 'solid',
      borderColor: cssVar('border'),
      borderRadius: cssVar('radiusLg'),
      padding: '0.75rem',
      backgroundColor: cssVar('surfaceSecondary'),
    }}>
      <div style={{ fontSize: '0.8125rem', fontWeight: 600, color: cssVar('text'), marginBottom: '0.25rem' }}>
        Agent Identity 凭证
      </div>
      <div style={{ ...descStyle, marginTop: 0, marginBottom: '0.75rem' }}>
        从新版 Sub2API 导入的 Agent Identity 账号只需要以下签名凭证，首次请求时会自动注册 task。
      </div>
      <div style={{ marginBottom: '0.75rem' }}>
        <label style={labelStyle}>认证模式</label>
        <input
          type="text"
          autoComplete="off"
          style={inputStyle}
          placeholder="agentIdentity"
          value={credentials.auth_mode ?? ''}
          onChange={(e) => updateField('auth_mode', e.target.value)}
        />
      </div>
      <div style={{ marginBottom: '0.75rem' }}>
        <label style={labelStyle}>Agent Runtime ID</label>
        <input
          type="text"
          autoComplete="off"
          style={inputStyle}
          placeholder="Agent Identity 导入后自动填充"
          value={credentials.agent_runtime_id ?? ''}
          onChange={(e) => updateField('agent_runtime_id', e.target.value)}
        />
      </div>
      <div style={{ marginBottom: '0.75rem' }}>
        <label style={labelStyle}>Agent Private Key</label>
        <SecretInput
          placeholder="Base64 PKCS#8 Ed25519 私钥"
          value={credentials.agent_private_key ?? ''}
          onChange={(e) => updateField('agent_private_key', e.target.value)}
        />
      </div>
      <div style={{ marginBottom: '0.75rem' }}>
        <label style={labelStyle}>Agent Task ID</label>
        <input
          type="text"
          autoComplete="off"
          style={inputStyle}
          placeholder="留空则首次请求自动注册"
          value={credentials.task_id ?? ''}
          onChange={(e) => updateField('task_id', e.target.value)}
        />
      </div>
      <div>
        <label style={labelStyle}>ChatGPT User ID</label>
        <input
          type="text"
          autoComplete="off"
          style={inputStyle}
          placeholder="Agent Identity 导入后自动填充"
          value={credentials.chatgpt_user_id ?? ''}
          onChange={(e) => updateField('chatgpt_user_id', e.target.value)}
        />
      </div>
    </div>
  );

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      {/* 账号类型选择（编辑模式下只读） */}
      <div>
        <span style={labelStyle}>账号类型 {mode === 'create' ? '*' : ''}</span>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '0.75rem' }}>
          <div
            style={{
              ...(accountType === 'apikey' ? cardActiveStyle : cardStyle),
              ...(mode === 'edit' && accountType !== 'apikey' ? { opacity: 0.4, cursor: 'not-allowed' } : {}),
            }}
            onClick={mode === 'create' ? () => handleTypeChange('apikey') : undefined}
          >
            <div style={accountTypeTitleStyle}>API Key</div>
            <div style={accountTypeDescStyle}>支持所有 Responses 标准接口</div>
          </div>
          <div
            style={{
              ...(accountType === 'oauth' ? cardActiveStyle : cardStyle),
              ...(mode === 'edit' && accountType !== 'oauth' ? { opacity: 0.4, cursor: 'not-allowed' } : {}),
            }}
            onClick={mode === 'create' ? () => handleTypeChange('oauth') : undefined}
          >
            <div style={accountTypeTitleStyle}>OAuth 登录</div>
            <div style={accountTypeDescStyle}>支持浏览器授权 / Refresh Token / Session 导入</div>
          </div>
        </div>
      </div>

      {accountType === 'apikey' && mode === 'create' && (
        <>
          <div>
            <label style={labelStyle}>API 地址</label>
            <input
              type="text"
              style={inputStyle}
              placeholder="https://api.openai.com"
              value={credentials.base_url ?? ''}
              onChange={(e) => updateField('base_url', e.target.value)}
            />
            <div style={{ ...descStyle, marginTop: '0.375rem' }}>
              留空使用默认地址，支持自定义反向代理
            </div>
          </div>
          <div>
            <label style={labelStyle}>
              API Key <span style={{ color: cssVar('danger') }}>*</span>
            </label>
            <SecretInput
              name="api_key"
              placeholder="sk-..."
              value={credentials.api_key ?? ''}
              onChange={(e) => updateField('api_key', e.target.value)}
            />
          </div>
        </>
      )}

      {accountType === 'oauth' && (
        <>
          {/* 订阅信息展示 */}
          {(planType || subscriptionUntil) && (
            <div style={{
              borderWidth: '1px',
              borderStyle: 'solid',
              borderColor: cssVar('border'),
              borderRadius: cssVar('radiusLg'),
              padding: '0.5rem 0.75rem',
              backgroundColor: cssVar('surfaceSecondary'),
              display: 'flex',
              alignItems: 'center',
              gap: '0.5rem',
              flexWrap: 'wrap',
            }}>
              <div style={{ fontSize: '0.6875rem', fontWeight: 500, color: cssVar('textSecondary'), textTransform: 'uppercase', letterSpacing: '0.05em', lineHeight: 1.2 }}>
                订阅
              </div>
              {planType && (() => {
                const plan = planDisplayMap[planType] || { label: planType, color: cssVar('text'), bg: cssVar('surfaceSecondary') };
                return (
                  <span style={{
                    display: 'inline-block',
                    padding: '0.0625rem 0.375rem',
                    borderRadius: '9999px',
                    fontSize: '0.6875rem',
                    fontWeight: 600,
                    lineHeight: 1.2,
                    color: plan.color,
                    backgroundColor: plan.bg,
                  }}>
                    {plan.label}
                  </span>
                );
              })()}
              {subscriptionUntil && (
                <span style={{ fontSize: '0.6875rem', color: cssVar('textTertiary'), lineHeight: 1.2 }}>
                  有效期至 {subscriptionUntil}
                </span>
              )}
            </div>
          )}

          {oauth && mode === 'create' && (
            <div style={{ borderWidth: '1px', borderStyle: 'solid', borderColor: cssVar('border'), borderRadius: cssVar('radiusLg'), padding: '1rem', backgroundColor: cssVar('surfaceSecondary') }}>
              <div style={{ fontSize: '0.875rem', fontWeight: 600, color: cssVar('text'), marginBottom: '0.5rem' }}>
                授权方式
              </div>
              <div style={{ display: 'flex', gap: '0.5rem', flexWrap: 'wrap', marginBottom: '0.875rem' }}>
                <span
                  style={oauthMode === 'browser' ? pillActiveStyle : pillStyle}
                  onClick={() => setOauthMode('browser')}
                >
                  浏览器授权
                </span>
                {oauth.importRefresh && (
                  <span
                    style={oauthMode === 'refresh_single' ? pillActiveStyle : pillStyle}
                    onClick={() => setOauthMode('refresh_single')}
                  >
                    Refresh Token 导入
                  </span>
                )}
                {oauth.batchImportRefresh && onBatchImport && (
                  <span
                    style={oauthMode === 'refresh_batch' ? pillActiveStyle : pillStyle}
                    onClick={() => setOauthMode('refresh_batch')}
                  >
                    RT 批量导入
                  </span>
                )}
                {oauthExt?.importSession && (
                  <span
                    style={oauthMode === 'session_single' ? pillActiveStyle : pillStyle}
                    onClick={() => setOauthMode('session_single')}
                  >
                    Session 导入
                  </span>
                )}
                {oauthExt?.batchImportSession && onBatchImport && (
                  <span
                    style={oauthMode === 'session_batch' ? pillActiveStyle : pillStyle}
                    onClick={() => setOauthMode('session_batch')}
                  >
                    Session 批量导入
                  </span>
                )}
              </div>

              {oauthMode === 'browser' && (
                <>
                  <div style={{ ...descStyle, marginTop: 0, marginBottom: '0.75rem' }}>
                    先生成授权链接，在浏览器完成授权后，把完整回调 URL 粘贴回来完成交换。
                  </div>
                  <div style={{ display: 'flex', gap: '0.75rem', marginBottom: '0.75rem', flexWrap: 'wrap' }}>
                    <button type="button" onClick={startOAuth} disabled={oauthLoading} style={primaryBtnStyle(oauthLoading)}>
                      生成授权链接
                    </button>
                    <button type="button" onClick={copyAuthorizeURL} disabled={!authorizeURL || oauthLoading} style={ghostBtnStyle(!authorizeURL || oauthLoading)}>
                      复制授权链接
                    </button>
                  </div>
                  <div style={{ marginBottom: '0.75rem' }}>
                    <label style={labelStyle}>授权链接</label>
                    <textarea
                      style={{ ...inputStyle, minHeight: '120px', resize: 'vertical' }}
                      readOnly
                      placeholder='点击"生成授权链接"后，这里会显示完整授权地址'
                      value={authorizeURL}
                    />
                  </div>
                  <div style={{ marginBottom: '0.75rem' }}>
                    <label style={labelStyle}>回调 URL</label>
                    <textarea
                      style={{ ...inputStyle, minHeight: '76px', resize: 'vertical' }}
                      placeholder="粘贴完整回调 URL，例如 http://localhost:1455/auth/callback?code=...&state=..."
                      value={callbackURL}
                      onChange={(e) => setCallbackURL(e.target.value)}
                    />
                  </div>
                  <div style={{ display: 'flex', gap: '0.75rem', alignItems: 'center', flexWrap: 'wrap' }}>
                    <button
                      type="button"
                      onClick={submitOAuthCallback}
                      disabled={!callbackURL.trim() || oauthLoading}
                      style={outlineBtnStyle(!callbackURL.trim() || oauthLoading)}
                    >
                      完成授权交换
                    </button>
                    <StatusMessage status={oauthStatus} />
                  </div>
                </>
              )}

              {oauthMode === 'refresh_single' && (
                <>
                  <div style={{ ...descStyle, marginTop: 0, marginBottom: '0.75rem' }}>
                    粘贴已有的 Refresh Token，后台会自动刷新拿回 access_token 并解析用户信息。
                  </div>
                  {refreshTokenTypeField}
                  <div style={{ marginBottom: '0.75rem' }}>
                    <label style={labelStyle}>Refresh Token</label>
                    <textarea
                      style={{ ...inputStyle, minHeight: '90px', resize: 'vertical' }}
                      placeholder="粘贴单个 Refresh Token"
                      value={refreshTokenInput}
                      onChange={(e) => setRefreshTokenInput(e.target.value)}
                      autoComplete="off"
                    />
                  </div>
                  <div style={{ display: 'flex', gap: '0.75rem', alignItems: 'center', flexWrap: 'wrap' }}>
                    <button
                      type="button"
                      onClick={submitRefreshTokenImport}
                      disabled={!refreshTokenInput.trim() || oauthLoading}
                      style={primaryBtnStyle(!refreshTokenInput.trim() || oauthLoading)}
                    >
                      导入
                    </button>
                    <StatusMessage status={oauthStatus} />
                  </div>
                </>
              )}

              {oauthMode === 'refresh_batch' && (
                <>
                  {batchPhase === 'input' && (
                    <>
                      <div style={{ ...descStyle, marginTop: 0, marginBottom: '0.75rem' }}>
                        每行一个 Refresh Token，批量换取凭证并一键创建账号（# 开头的行视为注释）。
                      </div>
                      {refreshTokenTypeField}
                      <div style={{ marginBottom: '0.75rem' }}>
                        <label style={labelStyle}>Refresh Tokens</label>
                        <textarea
                          style={{ ...inputStyle, minHeight: '180px', resize: 'vertical', fontFamily: 'monospace' }}
                          placeholder={'每行一个 Refresh Token\n# 以 # 开头的行会被忽略'}
                          value={batchText}
                          onChange={(e) => setBatchText(e.target.value)}
                          autoComplete="off"
                        />
                      </div>
                      <div style={{ display: 'flex', gap: '0.75rem', alignItems: 'center', flexWrap: 'wrap' }}>
                        <button
                          type="button"
                          onClick={submitBatchRefreshImport}
                          disabled={batchRefreshTokens.length === 0}
                          style={primaryBtnStyle(batchRefreshTokens.length === 0)}
                        >
                          批量导入 ({batchRefreshTokens.length})
                        </button>
                        <StatusMessage status={oauthStatus} />
                      </div>
                    </>
                  )}

                  {batchPhase === 'running' && (
                    <div style={{ fontSize: '0.875rem', color: cssVar('textSecondary') }}>
                      正在换取并导入，请稍候...
                    </div>
                  )}

                  {batchPhase === 'result' && (
                    <>
                      <div style={{ fontSize: '0.875rem', color: cssVar('text'), marginBottom: '0.75rem' }}>
                        共 {batchResults.length} 个，成功 {batchImportedCount} 个，失败 {batchResults.filter((r) => r.status === 'failed').length} 个
                      </div>
                      <div style={{
                        maxHeight: '280px',
                        overflowY: 'auto',
                        borderWidth: '1px',
                        borderStyle: 'solid',
                        borderColor: cssVar('border'),
                        borderRadius: cssVar('radiusMd'),
                        padding: '0.5rem 0.75rem',
                        marginBottom: '0.75rem',
                      }}>
                        {batchResults.map((r, idx) => (
                          <div key={idx} style={{
                            fontSize: '0.75rem',
                            padding: '0.375rem 0',
                            borderBottom: idx < batchResults.length - 1 ? `1px solid ${cssVar('border')}` : 'none',
                            color: r.status === 'ok' ? cssVar('success') : cssVar('danger'),
                          }}>
                            <span style={{ fontWeight: 500 }}>
                              [{r.status === 'ok' ? '成功' : '失败'}]
                            </span>{' '}
                            {r.status === 'ok'
                              ? (r.accountName || r.credentials.email || '未知账号')
                              : (r.error || '未知错误')}
                          </div>
                        ))}
                      </div>
                      <button type="button" onClick={resetBatch} style={ghostBtnStyle(false)}>
                        再导入一批
                      </button>
                    </>
                  )}
                </>
              )}

              {oauthMode === 'session_single' && (
                <>
                  <div style={{ ...descStyle, marginTop: 0, marginBottom: '0.75rem' }}>
                    粘贴 chatgpt.com 的 /api/auth/session 完整 JSON，或仅 sessionToken（JWE 串）。
                    JSON 路径零网络调用，直接解析后入库；仅传 sessionToken 时会调一次 session 端点拉完整信息。
                  </div>
                  <div style={{ marginBottom: '0.75rem' }}>
                    <label style={labelStyle}>Session JSON 或 sessionToken</label>
                    <textarea
                      style={{ ...inputStyle, minHeight: '160px', resize: 'vertical', fontFamily: 'monospace' }}
                      placeholder={'{\n  "user": {...},\n  "accessToken": "eyJhbGciOi...",\n  "sessionToken": "eyJhbGciOiJkaXIi..."\n}'}
                      value={sessionInput}
                      onChange={(e) => setSessionInput(e.target.value)}
                      autoComplete="off"
                    />
                  </div>
                  <div style={{ display: 'flex', gap: '0.75rem', alignItems: 'center', flexWrap: 'wrap' }}>
                    <button
                      type="button"
                      onClick={submitSessionImport}
                      disabled={!sessionInput.trim() || oauthLoading}
                      style={primaryBtnStyle(!sessionInput.trim() || oauthLoading)}
                    >
                      导入
                    </button>
                    <StatusMessage status={oauthStatus} />
                  </div>
                </>
              )}

              {oauthMode === 'session_batch' && (
                <>
                  {batchPhase === 'input' && (
                    <>
                      <div style={{ ...descStyle, marginTop: 0, marginBottom: '0.75rem' }}>
                        每行一个 session JSON（单行）或一个 sessionToken。
                        如果你想贴一个跨行的 JSON，直接粘进去也行——会被识别为单条。# 开头视为注释。
                      </div>
                      <div style={{ marginBottom: '0.75rem' }}>
                        <label style={labelStyle}>Sessions</label>
                        <textarea
                          style={{ ...inputStyle, minHeight: '220px', resize: 'vertical', fontFamily: 'monospace' }}
                          placeholder={'每行一个 sessionToken 或一行一个完整 JSON\n# 以 # 开头的行会被忽略'}
                          value={sessionBatchText}
                          onChange={(e) => setSessionBatchText(e.target.value)}
                          autoComplete="off"
                        />
                      </div>
                      <div style={{ display: 'flex', gap: '0.75rem', alignItems: 'center', flexWrap: 'wrap' }}>
                        <button
                          type="button"
                          onClick={submitBatchSessionImport}
                          disabled={batchSessionItems.length === 0}
                          style={primaryBtnStyle(batchSessionItems.length === 0)}
                        >
                          批量导入 ({batchSessionItems.length})
                        </button>
                        <StatusMessage status={oauthStatus} />
                      </div>
                    </>
                  )}

                  {batchPhase === 'running' && (
                    <div style={{ fontSize: '0.875rem', color: cssVar('textSecondary') }}>
                      正在解析并导入，请稍候...
                    </div>
                  )}

                  {batchPhase === 'result' && (
                    <>
                      <div style={{ fontSize: '0.875rem', color: cssVar('text'), marginBottom: '0.75rem' }}>
                        共 {batchResults.length} 个，成功 {batchImportedCount} 个，失败 {batchResults.filter((r) => r.status === 'failed').length} 个
                      </div>
                      <div style={{
                        maxHeight: '280px',
                        overflowY: 'auto',
                        borderWidth: '1px',
                        borderStyle: 'solid',
                        borderColor: cssVar('border'),
                        borderRadius: cssVar('radiusMd'),
                        padding: '0.5rem 0.75rem',
                        marginBottom: '0.75rem',
                      }}>
                        {batchResults.map((r, idx) => (
                          <div key={idx} style={{
                            fontSize: '0.75rem',
                            padding: '0.375rem 0',
                            borderBottom: idx < batchResults.length - 1 ? `1px solid ${cssVar('border')}` : 'none',
                            color: r.status === 'ok' ? cssVar('success') : cssVar('danger'),
                          }}>
                            <span style={{ fontWeight: 500 }}>
                              [{r.status === 'ok' ? '成功' : '失败'}]
                            </span>{' '}
                            {r.status === 'ok'
                              ? (r.accountName || r.credentials.email || '未知账号')
                              : (r.error || '未知错误')}
                          </div>
                        ))}
                      </div>
                      <button type="button" onClick={resetBatch} style={ghostBtnStyle(false)}>
                        再导入一批
                      </button>
                    </>
                  )}
                </>
              )}
            </div>
          )}

          {/* 非批量模式下展示凭证字段 */}
          {!isBatchActive && mode === 'create' && (
            <>
              <div>
                <label style={labelStyle}>
                  Access Token {!oauth && <span style={{ color: cssVar('danger') }}>*</span>}
                </label>
                <SecretInput
                  placeholder={oauth ? '授权后自动填充，或手动输入' : 'eyJhbG...'}
                  value={credentials.access_token ?? ''}
                  onChange={(e) => updateField('access_token', e.target.value)}
                />
              </div>
              <div>
                <label style={labelStyle}>Refresh Token</label>
                <SecretInput
                  placeholder="授权后自动填充"
                  value={credentials.refresh_token ?? ''}
                  onChange={(e) => updateField('refresh_token', e.target.value)}
                />
              </div>
              <div>
                <label style={labelStyle}>Session Token (JWE)</label>
                <SecretInput
                  placeholder="Session 导入后自动填充，或手动粘贴"
                  value={credentials.session_token ?? ''}
                  onChange={(e) => updateField('session_token', e.target.value)}
                />
              </div>
              <div>
                <label style={labelStyle}>Client ID</label>
                <input
                  type="text"
                  autoComplete="off"
                  style={inputStyle}
                  placeholder="留空使用默认 Codex Client ID"
                  value={credentials.client_id ?? ''}
                  onChange={(e) => updateField('client_id', e.target.value)}
                />
              </div>
              <div>
                <label style={labelStyle}>ChatGPT Account ID</label>
                <input
                  type="text"
                  style={inputStyle}
                  placeholder="授权后自动填充"
                  value={credentials.chatgpt_account_id ?? ''}
                  onChange={(e) => updateField('chatgpt_account_id', e.target.value)}
                />
              </div>
            </>
          )}

          {/* Agent Identity 凭证（仅 Agent Identity 账号展示） */}
          {!isBatchActive && mode === 'create' && agentIdentity && agentIdentityBlock}
        </>
      )}

      {/* 账号凭证区域：编辑模式下默认折叠，展开后标明凭证类型并可查看/修改凭证值 */}
      {mode === 'edit' && accountType && (
        <div style={{
          width: '100%',
          boxSizing: 'border-box',
          borderWidth: '1px',
          borderStyle: 'solid',
          borderColor: cssVar('border'),
          borderRadius: cssVar('radiusLg'),
          backgroundColor: cssVar('surfaceSecondary'),
          overflow: 'hidden',
        }}>
          <button
            type="button"
            aria-expanded={credentialsExpanded}
            onClick={() => setCredentialsExpanded((prev) => !prev)}
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: '0.5rem',
              flexWrap: 'wrap',
              width: '100%',
              boxSizing: 'border-box',
              padding: '0.5rem 0.75rem',
              border: 'none',
              backgroundColor: 'transparent',
              cursor: 'pointer',
              textAlign: 'left',
            }}
          >
            <span style={{ fontSize: '0.8125rem', fontWeight: 600, color: cssVar('text') }}>
              账号凭证
            </span>
            {credentialBadges.map((badge) => (
              <span
                key={badge}
                style={{
                  display: 'inline-block',
                  padding: '0.0625rem 0.375rem',
                  borderRadius: '9999px',
                  fontSize: '0.6875rem',
                  fontWeight: 600,
                  lineHeight: 1.4,
                  color: cssVar('primary'),
                  backgroundColor: cssVar('primarySubtle'),
                }}
              >
                {badge}
              </span>
            ))}
            <span
              aria-hidden="true"
              style={{
                marginLeft: 'auto',
                color: cssVar('textTertiary'),
                fontSize: '0.75rem',
                transform: credentialsExpanded ? 'rotate(180deg)' : 'none',
                transition: 'transform 0.15s',
              }}
            >
              ▾
            </span>
          </button>
          {credentialsExpanded && (
            <div style={{
              display: 'flex',
              flexDirection: 'column',
              gap: '0.75rem',
              padding: '0 0.75rem 0.75rem',
            }}>
              {accountType === 'apikey' && (
                <>
                  <div>
                    <label style={labelStyle}>API 地址</label>
                    <input
                      type="text"
                      style={inputStyle}
                      placeholder="https://api.openai.com"
                      value={credentials.base_url ?? ''}
                      onChange={(e) => updateField('base_url', e.target.value)}
                    />
                    <div style={{ ...descStyle, marginTop: '0.375rem' }}>
                      留空使用默认地址，支持自定义反向代理
                    </div>
                  </div>
                  <div>
                    <label style={labelStyle}>API Key</label>
                    <SecretInput
                      name="api_key"
                      placeholder="sk-..."
                      value={credentials.api_key ?? ''}
                      onChange={(e) => updateField('api_key', e.target.value)}
                    />
                  </div>
                </>
              )}
              {accountType === 'oauth' && (
                <>
                  {credentials.access_token && (
                    <div>
                      <label style={labelStyle}>Access Token</label>
                      <SecretInput
                        placeholder="未设置"
                        value={credentials.access_token ?? ''}
                        onChange={(e) => updateField('access_token', e.target.value)}
                      />
                    </div>
                  )}
                  {credentials.refresh_token && (
                    <div>
                      <label style={labelStyle}>Refresh Token</label>
                      <SecretInput
                        placeholder="未设置"
                        value={credentials.refresh_token ?? ''}
                        onChange={(e) => updateField('refresh_token', e.target.value)}
                      />
                    </div>
                  )}
                  {credentials.session_token && (
                    <div>
                      <label style={labelStyle}>Session Token (JWE)</label>
                      <SecretInput
                        placeholder="未设置"
                        value={credentials.session_token ?? ''}
                        onChange={(e) => updateField('session_token', e.target.value)}
                      />
                    </div>
                  )}
                  {agentIdentity && agentIdentityBlock}
                </>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
