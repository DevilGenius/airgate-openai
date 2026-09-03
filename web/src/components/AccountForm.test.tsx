import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { AccountForm } from './AccountForm';
import type { AccountFormProps } from '@devilgenius/airgate-theme/plugin';

function Harness({
  accountType,
  credentials = {},
  mode = 'create',
  oauth,
  onBatchImport,
  onBatchModeChange = vi.fn(),
  onChange = vi.fn(),
  onSuggestedName = vi.fn(),
  onTypeChange = vi.fn(),
}: {
  accountType?: string;
  credentials?: Record<string, string>;
  mode?: AccountFormProps['mode'];
  oauth?: AccountFormProps['oauth'];
  onBatchImport?: AccountFormProps['onBatchImport'];
  onBatchModeChange?: NonNullable<AccountFormProps['onBatchModeChange']>;
  onChange?: NonNullable<AccountFormProps['onChange']>;
  onSuggestedName?: NonNullable<AccountFormProps['onSuggestedName']>;
  onTypeChange?: NonNullable<AccountFormProps['onAccountTypeChange']>;
}) {
  const [currentCredentials, setCurrentCredentials] = useState(credentials);
  const [currentType, setCurrentType] = useState(accountType);

  return (
    <AccountForm
      accountType={currentType}
      credentials={currentCredentials}
      mode={mode}
      oauth={oauth}
      onBatchImport={onBatchImport}
      onBatchModeChange={onBatchModeChange}
      onSuggestedName={onSuggestedName}
      onAccountTypeChange={(next) => {
        setCurrentType(next);
        onTypeChange(next);
      }}
      onChange={(next) => {
        setCurrentCredentials(next);
        onChange(next);
      }}
    />
  );
}

function oauthBridge(overrides: Record<string, unknown> = {}) {
  return {
    batchExchange: vi.fn(),
    batchImportRefresh: vi.fn(),
    batchImportSession: vi.fn(),
    exchange: vi.fn(),
    importRefresh: vi.fn(),
    importSession: vi.fn(),
    start: vi.fn(),
    ...overrides,
  };
}

describe('OpenAI AccountForm', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('shows ProLite and keeps its supplied expiry visible', () => {
    render(<Harness
      accountType="oauth"
      mode="edit"
      credentials={{
        plan_type: 'Self_serve_business_prolite',
        subscription_active_until: '2020-01-01T00:00:00Z',
      }}
    />);

    expect(screen.getByText('ProLite')).toBeInTheDocument();
    expect(screen.getByText('有效期至 2020-01-01T00:00:00Z')).toBeInTheDocument();
  });

  it('switches to API key mode and updates key/base URL credentials', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const onTypeChange = vi.fn();

    render(<Harness onChange={onChange} onTypeChange={onTypeChange} />);

    await user.click(screen.getByText('API Key'));
    await user.type(screen.getByPlaceholderText('sk-...'), 'sk-openai');
    await user.type(screen.getByPlaceholderText('https://api.openai.com'), 'https://proxy.example');

    expect(onTypeChange).toHaveBeenCalledWith('apikey');
    expect(onChange).toHaveBeenLastCalledWith({
      api_key: 'sk-openai',
      base_url: 'https://proxy.example',
      provider: '',
    });
  });

  it('runs browser OAuth start, copy and callback exchange flow', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const onSuggestedName = vi.fn();
    const onTypeChange = vi.fn();
    const oauth = oauthBridge({
      exchange: vi.fn().mockResolvedValue({
        accountName: 'OpenAI User',
        accountType: 'oauth',
        credentials: { access_token: 'access', refresh_token: 'refresh' },
      }),
      start: vi.fn().mockResolvedValue({ authorizeURL: 'https://auth.example', state: 'state-1' }),
    });

    render(
      <Harness
        oauth={oauth}
        onChange={onChange}
        onSuggestedName={onSuggestedName}
        onTypeChange={onTypeChange}
      />,
    );

    await user.click(screen.getByText('OAuth 登录'));
    await user.click(screen.getByRole('button', { name: '生成授权链接' }));
    expect(await screen.findByDisplayValue('https://auth.example')).toBeTruthy();

    const writeText = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue(undefined);
    await user.click(screen.getByRole('button', { name: '复制授权链接' }));
    expect(writeText).toHaveBeenCalledWith('https://auth.example');

    await user.type(screen.getByPlaceholderText(/粘贴完整回调 URL/), 'http://localhost/callback?code=ok');
    await user.click(screen.getByRole('button', { name: '完成授权交换' }));

    await waitFor(() => expect(oauth.exchange).toHaveBeenCalledWith('http://localhost/callback?code=ok'));
    expect(onTypeChange).toHaveBeenLastCalledWith('oauth');
    expect(onSuggestedName).toHaveBeenCalledWith('OpenAI User');
    expect(onChange).toHaveBeenLastCalledWith({
      access_token: 'access',
      base_url: '',
      provider: '',
      refresh_token: 'refresh',
      session_token: '',
      chatgpt_account_id: '',
    });
  });

  it('imports a mobile refresh token through the OAuth bridge', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const onSuggestedName = vi.fn();
    const oauth = oauthBridge({
      importRefresh: vi.fn().mockResolvedValue({
        accountName: 'Mobile User',
        accountType: 'oauth',
        credentials: { access_token: 'mobile-access', refresh_token: 'mobile-refresh' },
      }),
    });

    render(<Harness oauth={oauth} onChange={onChange} onSuggestedName={onSuggestedName} />);

    await user.click(screen.getByText('OAuth 登录'));
    await user.click(screen.getByText('Refresh Token 导入'));
    await user.selectOptions(screen.getByDisplayValue('普通 RT'), 'mobile');
    await user.type(screen.getByPlaceholderText('粘贴单个 Refresh Token'), 'rt-mobile');
    await user.click(screen.getByRole('button', { name: '导入' }));

    await waitFor(() => expect(oauth.importRefresh).toHaveBeenCalledWith('rt-mobile', expect.stringMatching(/^app_/)));
    expect(onSuggestedName).toHaveBeenCalledWith('Mobile User');
    expect(onChange).toHaveBeenLastCalledWith(expect.objectContaining({
      access_token: 'mobile-access',
      refresh_token: 'mobile-refresh',
    }));
  });

  it('batch imports refresh tokens and sends successful accounts to core', async () => {
    const user = userEvent.setup();
    const onBatchImport = vi.fn().mockResolvedValue({ failed: 0, imported: 1 });
    const onBatchModeChange = vi.fn();
    const oauth = oauthBridge({
      batchImportRefresh: vi.fn().mockResolvedValue([
        {
          accountName: 'Batch User',
          accountType: 'oauth',
          credentials: { access_token: 'batch-access', email: 'batch@example.com' },
          status: 'ok',
        },
        { error: 'bad token', status: 'failed' },
      ]),
    });

    render(<Harness oauth={oauth} onBatchImport={onBatchImport} onBatchModeChange={onBatchModeChange} />);

    await user.click(screen.getByText('OAuth 登录'));
    await user.click(screen.getByText('RT 批量导入'));
    await user.type(screen.getByPlaceholderText(/每行一个 Refresh Token/), 'rt-1\n# ignored\nrt-2');
    await user.click(screen.getByRole('button', { name: '批量导入 (2)' }));

    await waitFor(() => expect(oauth.batchImportRefresh).toHaveBeenCalledWith(['rt-1', 'rt-2'], undefined));
    expect(onBatchImport).toHaveBeenCalledWith([
      {
        credentials: { access_token: 'batch-access', email: 'batch@example.com' },
        name: 'Batch User',
        type: 'oauth',
      },
    ]);
    expect(await screen.findByText(/共 2 个，成功 1 个，失败 1 个/)).toBeTruthy();
    expect(onBatchModeChange).toHaveBeenCalledWith(true);
  });

  it('imports a single session payload through the extended OAuth bridge', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const onSuggestedName = vi.fn();
    const oauth = oauthBridge({
      importSession: vi.fn().mockResolvedValue({
        accountName: 'Session User',
        accountType: 'oauth',
        credentials: { access_token: 'session-access', session_token: 'session-token' },
      }),
    });

    render(<Harness oauth={oauth} onChange={onChange} onSuggestedName={onSuggestedName} />);

    await user.click(screen.getByText('OAuth 登录'));
    await user.click(screen.getByText('Session 导入'));
    fireEvent.change(screen.getByPlaceholderText(/"accessToken"/), {
      target: { value: '{"accessToken":"access","sessionToken":"session"}' },
    });
    await user.click(screen.getByRole('button', { name: '导入' }));

    await waitFor(() => expect(oauth.importSession).toHaveBeenCalledWith('{"accessToken":"access","sessionToken":"session"}'));
    expect(onSuggestedName).toHaveBeenCalledWith('Session User');
    expect(onChange).toHaveBeenLastCalledWith(expect.objectContaining({
      access_token: 'session-access',
      session_token: 'session-token',
    }));
  });

  it('batch imports session payloads and resets the result view', async () => {
    const user = userEvent.setup();
    const onBatchImport = vi.fn().mockResolvedValue({ failed: 0, imported: 2 });
    const oauth = oauthBridge({
      batchImportSession: vi.fn().mockResolvedValue([
        {
          accountName: 'Session One',
          accountType: 'oauth',
          credentials: { access_token: 'one', email: 'one@example.com' },
          status: 'ok',
        },
        {
          accountName: '',
          accountType: 'oauth',
          credentials: { access_token: 'two', email: 'two@example.com' },
          status: 'ok',
        },
      ]),
    });

    render(<Harness oauth={oauth} onBatchImport={onBatchImport} />);

    await user.click(screen.getByText('OAuth 登录'));
    await user.click(screen.getByText('Session 批量导入'));
    fireEvent.change(screen.getByPlaceholderText(/每行一个 sessionToken/), {
      target: { value: 'session-1\n# ignored\nsession-2' },
    });
    await user.click(screen.getByRole('button', { name: '批量导入 (2)' }));

    await waitFor(() => expect(oauth.batchImportSession).toHaveBeenCalledWith(['session-1', 'session-2']));
    expect(onBatchImport).toHaveBeenCalledWith([
      { credentials: { access_token: 'one', email: 'one@example.com' }, name: 'Session One', type: 'oauth' },
      { credentials: { access_token: 'two', email: 'two@example.com' }, name: 'two@example.com', type: 'oauth' },
    ]);
    expect(await screen.findByText(/共 2 个，成功 2 个，失败 0 个/)).toBeTruthy();

    await user.click(screen.getByRole('button', { name: '再导入一批' }));
    expect(screen.getByRole('button', { name: '批量导入 (0)' })).toBeDisabled();
  });

  it('updates editable OAuth refresh and session tokens', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();

    render(
      <Harness
        accountType="oauth"
        credentials={{ refresh_token: 'old-refresh', session_token: 'old-session' }}
        mode="edit"
        onChange={onChange}
      />,
    );

    expect(screen.queryByDisplayValue('old-refresh')).toBeNull();
    expect(screen.getByText('Refresh Token', { selector: 'span' })).toBeTruthy();
    expect(screen.getByText('Session', { selector: 'span' })).toBeTruthy();

    await user.click(screen.getByRole('button', { name: /账号凭证/ }));

    fireEvent.change(screen.getByDisplayValue('old-refresh'), { target: { value: 'new-refresh' } });
    fireEvent.change(screen.getByDisplayValue('old-session'), { target: { value: 'new-session' } });

    expect(onChange).toHaveBeenLastCalledWith({
      refresh_token: 'new-refresh',
      session_token: 'new-session',
    });
  });
});
