function planTokens(value?: string) {
  return (value || '').trim().toLowerCase().split(/[^a-z0-9]+/).filter(Boolean);
}

export function planUsesSubscriptionExpiry(value?: string) {
  const tokens = planTokens(value);
  return tokens.includes('plus') || tokens.includes('pro') || tokens.includes('professional');
}

export function accountPlanLabel(value: string) {
  const normalized = value.trim().toLowerCase();
  if (normalized === 'prolite' || normalized === 'self_serve_business_prolite') {
    return 'ProLite';
  }
  return value ? value.charAt(0).toUpperCase() + value.slice(1) : value;
}
