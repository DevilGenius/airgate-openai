import '@testing-library/jest-dom/vitest';
import { cleanup } from '@testing-library/react';
import { afterEach, vi } from 'vitest';

if (!navigator.clipboard) {
  Object.defineProperty(navigator, 'clipboard', {
    configurable: true,
    value: {
      writeText: vi.fn().mockResolvedValue(undefined),
    },
  });
}

if (!document.execCommand) {
  document.execCommand = vi.fn(() => true);
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});
