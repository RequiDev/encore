// Flat ESLint configuration.
//
// Three plugins, each earning its place: typescript-eslint for the language,
// react-hooks because the rules of hooks are not something review catches
// reliably, and jsx-a11y because accessibility in Encore is a floor rather than
// a feature and a floor needs a check that runs on every commit.

import js from '@eslint/js'
import globals from 'globals'
import tseslint from 'typescript-eslint'
import reactHooks from 'eslint-plugin-react-hooks'
import jsxA11y from 'eslint-plugin-jsx-a11y'

export default tseslint.config(
  { ignores: ['dist/**', 'node_modules/**', 'coverage/**'] },

  js.configs.recommended,
  ...tseslint.configs.recommended,

  {
    files: ['**/*.{ts,tsx}'],
    languageOptions: {
      ecmaVersion: 2023,
      globals: { ...globals.browser },
      parserOptions: { ecmaFeatures: { jsx: true } },
    },
    plugins: { 'jsx-a11y': jsxA11y },
    rules: {
      ...jsxA11y.flatConfigs.recommended.rules,

      // An unused argument named with a leading underscore is a deliberate
      // signature match, not a mistake.
      '@typescript-eslint/no-unused-vars': [
        'error',
        { argsIgnorePattern: '^_', varsIgnorePattern: '^_' },
      ],
      // `any` defeats the point of the typed API contract in lib/types.ts.
      '@typescript-eslint/no-explicit-any': 'error',
      '@typescript-eslint/consistent-type-imports': [
        'error',
        { prefer: 'type-imports', fixStyle: 'separate-type-imports' },
      ],
      // A floating promise in a click handler swallows its own rejection.
      'no-console': ['warn', { allow: ['warn', 'error'] }],
      eqeqeq: ['error', 'smart'],
    },
  },

  // The rules of hooks, from the plugin's own flat configuration.
  reactHooks.configs['recommended-latest'],

  {
    files: ['**/*.test.{ts,tsx}', 'src/test/**/*.ts'],
    languageOptions: { globals: { ...globals.browser, ...globals.node } },
  },

  {
    files: ['*.js', '*.ts', 'vite.config.ts'],
    languageOptions: { globals: { ...globals.node } },
  },
)
