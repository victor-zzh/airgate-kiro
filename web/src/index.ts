import { AccountForm } from './components/AccountForm';
import { UsageWindow } from './components/UsageWindow';
import type { PluginFrontendModule } from '@doudou-start/airgate-theme/plugin';

const plugin: PluginFrontendModule = {
  accountCreate: AccountForm,
  accountEdit: AccountForm,
  accountUsageWindow: UsageWindow,
};

export default plugin;
