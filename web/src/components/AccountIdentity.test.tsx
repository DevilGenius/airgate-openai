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
      context={{ credentials: { plan_type: 'Self_serve_business_prolite' } }}
    />);

    expect(screen.getByText('OAuth')).toBeInTheDocument();
    expect(screen.getByText('ProLite')).toBeInTheDocument();
    expect(screen.queryByText('Self_serve_business_prolite')).not.toBeInTheDocument();
  });
});
