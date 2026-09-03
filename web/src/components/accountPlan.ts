function planTokens(value?: string) {
  return (value || '').trim().toLowerCase().split(/[^a-z0-9]+/).filter(Boolean);
}

export function normalizeAccountPlan(value?: string) {
  const tokens = planTokens(value);
  const compact = tokens.join('');
  if (compact.endsWith('prolite')) return 'prolite';
  for (const token of tokens) {
    if (['free', 'plus', 'pro', 'team', 'k12', 'enterprise'].includes(token)) return token;
    if (token === 'professional') return 'pro';
  }
  return (value || '').trim().toLowerCase();
}

export function planUsesSubscriptionExpiry(value?: string) {
  const plan = normalizeAccountPlan(value);
  return plan === 'plus' || plan === 'pro';
}

export function accountPlanLabel(value: string) {
  const labels: Record<string, string> = {
    free: 'Free',
    plus: 'Plus',
    pro: 'Pro',
    team: 'Team',
    k12: 'K12',
    prolite: 'ProLite',
    enterprise: 'Enterprise',
  };
  const normalized = normalizeAccountPlan(value);
  if (labels[normalized]) {
    return labels[normalized];
  }
  return value ? value.charAt(0).toUpperCase() + value.slice(1) : value;
}
