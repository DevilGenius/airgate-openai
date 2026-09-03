import type { CSSProperties } from 'react';
import type { AccountSurfaceProps } from '@devilgenius/airgate-theme/plugin';
import { accountPlanLabel, planUsesSubscriptionExpiry } from './accountPlan';

type AccountLike = {
  type?: string;
  credentials?: Record<string, string>;
};

type DisplayOverrides = {
  plan_type?: unknown;
};

function readAccount(context: AccountSurfaceProps['context']): AccountLike {
  const account = context?.account;
  if (account && typeof account === 'object') return account as AccountLike;
  return {};
}

function readDisplayPlan(context: AccountSurfaceProps['context']) {
  const overrides = context?.display_overrides;
  if (!overrides || typeof overrides !== 'object' || Array.isArray(overrides)) return '';
  const planType = (overrides as DisplayOverrides).plan_type;
  return typeof planType === 'string' ? planType.trim() : '';
}

function typeLabel(type?: string) {
  if (type === 'oauth') return 'OAuth';
  if (type === 'apikey') return 'API Key';
  if (type === 'session_key') return 'Session Key';
  return type || '';
}

function isAgentIdentity(credentials: Record<string, string>) {
  const mode = (credentials.auth_mode || credentials.authMode || '')
    .trim()
    .toLowerCase()
    .replace(/_/g, '');
  return mode === 'agentidentity' || Boolean(credentials.agent_private_key || credentials.agent_runtime_id);
}

const rowStyle: CSSProperties = {
  display: 'flex',
  maxWidth: '100%',
  alignItems: 'center',
  justifyContent: 'center',
  gap: '0.25rem',
};

const typeBadgeStyle: CSSProperties = {
  maxWidth: '100%',
  overflow: 'hidden',
  border: '1px solid var(--ag-glass-border)',
  borderRadius: '0.25rem',
  background: 'var(--ag-bg-surface)',
  padding: '0 0.25rem',
  color: 'var(--ag-text-secondary)',
  fontSize: '0.625rem',
  lineHeight: 1,
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

const planBadgeStyle: CSSProperties = {
  maxWidth: '100%',
  overflow: 'hidden',
  borderRadius: '0.25rem',
  background: 'var(--ag-primary)',
  padding: '0 0.25rem',
  color: 'var(--ag-text-inverse)',
  fontSize: '0.625rem',
  fontWeight: 500,
  lineHeight: 1,
  opacity: 0.85,
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
};

export function AccountIdentity({ accountType, context }: AccountSurfaceProps) {
  const account = readAccount(context);
  const credentials = (context?.credentials as Record<string, string> | undefined) ?? account.credentials ?? {};
  const type = account.type || accountType;
  const displayType = type === 'oauth' && isAgentIdentity(credentials) ? 'Identity' : typeLabel(type);
  const planType = credentials.plan_type;
  const forcedDisplayPlan = readDisplayPlan(context);
  const subscriptionUntil = credentials.subscription_active_until;
  const subscriptionExpiryApplies = planUsesSubscriptionExpiry(planType);
  const subscriptionExpired = subscriptionUntil && subscriptionExpiryApplies
    ? new Date(subscriptionUntil) < new Date()
    : false;
  const hasQuotaMetadata = type === 'oauth' && (
    planType !== undefined || credentials.email !== undefined || subscriptionUntil !== undefined
  );
  const rawDisplayPlan = forcedDisplayPlan || planType || (hasQuotaMetadata ? 'free' : '');
  const displayPlan = !forcedDisplayPlan && rawDisplayPlan && subscriptionExpired && rawDisplayPlan.toLowerCase() !== 'free'
    ? 'free'
    : rawDisplayPlan;
  const isPaid = displayPlan && displayPlan.toLowerCase() !== 'free';
  const planTitle = isPaid && subscriptionUntil
    ? `过期时间：${new Date(subscriptionUntil).toLocaleDateString()}`
    : undefined;

  return (
    <div style={rowStyle}>
      {type && <span style={typeBadgeStyle}>{displayType}</span>}
      {displayPlan && (
        <span style={planBadgeStyle} title={planTitle}>
          {accountPlanLabel(displayPlan)}
        </span>
      )}
    </div>
  );
}
