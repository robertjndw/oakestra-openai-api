import './addon.css';

import { defineAddon } from '@oakestra/dashboard-sdk';
import { ServerCogIcon } from 'lucide-react';
import { ADDON_ID } from 'virtual:oakestra-addon';

import { KeysPage } from './pages/KeysPage';
import { ModelsPage } from './pages/ModelsPage';
import { OverviewPage } from './pages/OverviewPage';
import { PlacementPage } from './pages/PlacementPage';
import { SettingsPage } from './pages/SettingsPage';

export default defineAddon({
  id: ADDON_ID,
  label: 'OpenAI API',
  routes: [
    { index: true, Component: OverviewPage },
    { path: 'models', crumb: 'Models', Component: ModelsPage },
    { path: 'placement', crumb: 'Placement', fullWidth: true, Component: PlacementPage },
    // roles keep other users off these pages, including by a typed URL; the backend still
    // enforces admin on every call.
    { path: 'keys', crumb: 'API keys', roles: ['Admin'], Component: KeysPage },
    { path: 'settings', crumb: 'Settings', roles: ['Admin'], Component: SettingsPage },
  ],
  contributions: {
    'nav.items': [
      {
        id: 'main',
        label: 'OpenAI API',
        path: '',
        icon: ServerCogIcon,
        children: [
          { id: 'overview', label: 'Overview', path: '' },
          { id: 'models', label: 'Models', path: 'models' },
          { id: 'placement', label: 'Placement', path: 'placement' },
          { id: 'keys', label: 'API keys', path: 'keys', roles: ['Admin'] },
          { id: 'settings', label: 'Settings', path: 'settings', roles: ['Admin'] },
        ],
      },
    ],
  },
});
