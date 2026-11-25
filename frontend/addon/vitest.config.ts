import { defineConfig, mergeConfig } from 'vitest/config';

import viteConfig from './vite.config.ts';

// Built on vite.config.ts so `virtual:oakestra-addon` resolves in tests too. Module Federation
// switches itself off under Vitest.
export default mergeConfig(
  viteConfig,
  defineConfig({
    resolve: {
      // The linked SDK and @oakestra/ui sources would otherwise resolve these from the dashboard
      // checkout, giving the test host and our hooks different React, router and query contexts.
      dedupe: [
        'react',
        'react-dom',
        'react-router',
        '@tanstack/react-query',
        '@testing-library/react',
        '@testing-library/dom',
      ],
    },
    test: {
      environment: 'jsdom',
      // The Tailwind entry is built by the Vite plugin; tests do not need styles.
      css: false,
      // single-react first: it has to be in place before anything imports React.
      setupFiles: ['./test/single-react.ts', './test/setup.ts'],
    },
  }),
);
