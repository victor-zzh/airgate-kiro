import { AccountForm } from './components/AccountForm';
import type { AccountFormProps } from './components/AccountForm';

export interface PluginFrontendModule {
  accountForm?: React.ComponentType<AccountFormProps>;
}

const plugin: PluginFrontendModule = {
  accountForm: AccountForm,
};

export default plugin;
