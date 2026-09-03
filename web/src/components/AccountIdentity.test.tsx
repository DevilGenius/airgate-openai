import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { AccountIdentity } from './AccountIdentity';

describe('OpenAI AccountIdentity', () => {
  it('renders the real OAuth plan by default', () => {
    render(<AccountIdentity
      accountType="oauth"
      context={{ credentials: { plan_type: 'plus' } }}
    />);

    expect(screen.getByText('OAuth')).toBeInTheDocument();
    expect(screen.getByText('Plus')).toBeInTheDocument();
  });

  it('uses the frontend-only plan override when provided', () => {
    render(<AccountIdentity
      accountType="oauth"
      context={{
        credentials: { plan_type: 'plus' },
        display_overrides: { plan_type: 'pro' },
      }}
    />);

    expect(screen.getByText('OAuth')).toBeInTheDocument();
    expect(screen.getByText('Pro')).toBeInTheDocument();
    expect(screen.queryByText('Plus')).not.toBeInTheDocument();
  });

  it('displays the self-serve business prolite plan as ProLite', () => {
    render(<AccountIdentity
      accountType="oauth"
      context={{
        credentials: {
          plan_type: 'Self_serve_business_prolite',
          subscription_active_until: '2020-01-01T00:00:00Z',
        },
      }}
    />);

    expect(screen.getByText('OAuth')).toBeInTheDocument();
    expect(screen.getByText('ProLite')).toBeInTheDocument();
    expect(screen.queryByText('Self_serve_business_prolite')).not.toBeInTheDocument();
    expect(screen.queryByText('Free')).not.toBeInTheDocument();
  });

  it('only applies subscription expiry to Plus and Pro', () => {
    const expired = '2020-01-01T00:00:00Z';
    const { rerender } = render(<AccountIdentity
      accountType="oauth"
      context={{ credentials: { plan_type: 'team', subscription_active_until: expired } }}
    />);
    expect(screen.getByText('Team')).toBeInTheDocument();

    rerender(<AccountIdentity
      accountType="oauth"
      context={{ credentials: { plan_type: 'plus', subscription_active_until: expired } }}
    />);
    expect(screen.getByText('Free')).toBeInTheDocument();

    rerender(<AccountIdentity
      accountType="oauth"
      context={{ credentials: { plan_type: 'pro', subscription_active_until: expired } }}
    />);
    expect(screen.getByText('Free')).toBeInTheDocument();
  });
});
