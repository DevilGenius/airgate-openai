import { describe, expect, it } from 'vitest';
import { accountPlanLabel, normalizeAccountPlan, planUsesSubscriptionExpiry } from './accountPlan';

describe('OpenAI account plan presentation', () => {
  it.each([
    'prolite',
    'ProLite',
    'pro_lite',
    'pro lite',
    'ChatGPT ProLite',
    'Self_serve_business_prolite',
    'SELF_SERVE_BUSINESS_PRO_LITE',
  ])('normalizes and labels %s consistently', (value) => {
    expect(normalizeAccountPlan(value)).toBe('prolite');
    expect(accountPlanLabel(value)).toBe('ProLite');
    expect(planUsesSubscriptionExpiry(value)).toBe(false);
  });

  it('only uses subscription expiry for Plus and Pro', () => {
    expect(planUsesSubscriptionExpiry('ChatGPT Plus')).toBe(true);
    expect(planUsesSubscriptionExpiry('Builder Id Pro')).toBe(true);
    expect(planUsesSubscriptionExpiry('team')).toBe(false);
    expect(planUsesSubscriptionExpiry('k12')).toBe(false);
  });
});
