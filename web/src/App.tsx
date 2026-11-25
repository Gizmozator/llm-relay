import { Navigate, Route, Routes } from 'react-router-dom';
import { AuthProvider } from './auth';
import { FedRoleProvider } from './fedrole';
import { ToastProvider } from './components/Toast';
import Layout, { RequireAdmin, RequireAuth } from './Layout';
import LoginPage from './pages/Login';
import DashboardPage from './pages/Dashboard';
import UsagePage from './pages/Usage';
import FairnessPage from './pages/Fairness';
import KeysPage from './pages/Keys';
import AgentGuidePage from './pages/AgentGuide';
import SetupGuidePage from './pages/SetupGuide';
import BackendsPage from './pages/Backends';
import ModelsPage from './pages/Models';
import AdminModelsPage from './pages/AdminModels';
import AdminFederationPage from './pages/AdminFederation';
import UsersPage from './pages/Users';
import SettingsPage from './pages/Settings';

export default function App() {
  return (
    <ToastProvider>
      <AuthProvider>
        <FedRoleProvider>
          <Routes>
            <Route path="/login" element={<LoginPage />} />
            <Route
              element={
                <RequireAuth>
                  <Layout />
                </RequireAuth>
              }
            >
              <Route index element={<DashboardPage />} />
              <Route path="usage" element={<UsagePage admin={false} />} />
              <Route path="fairness" element={<FairnessPage />} />
              <Route path="keys" element={<KeysPage />} />
              <Route path="agents" element={<AgentGuidePage />} />
              <Route path="setup" element={<SetupGuidePage />} />
              <Route path="models" element={<ModelsPage />} />
              <Route
                path="admin/backends"
                element={
                  <RequireAdmin>
                    <BackendsPage />
                  </RequireAdmin>
                }
              />
              <Route
                path="admin/models"
                element={
                  <RequireAdmin>
                    <AdminModelsPage />
                  </RequireAdmin>
                }
              />
              <Route
                path="admin/users"
                element={
                  <RequireAdmin>
                    <UsersPage />
                  </RequireAdmin>
                }
              />
              <Route
                path="admin/usage"
                element={
                  <RequireAdmin>
                    <UsagePage admin={true} />
                  </RequireAdmin>
                }
              />
              <Route
                path="admin/federation"
                element={
                  <RequireAdmin>
                    <AdminFederationPage />
                  </RequireAdmin>
                }
              />
              {/* The Servers page merged into Federation (#7). The route stays
                  as a redirect so old bookmarks and links keep working; replace
                  keeps it out of the back-stack so the browser does not ping-pong
                  between the dead URL and the merged page. */}
              <Route path="admin/servers" element={<Navigate to="/admin/federation" replace />} />
              <Route
                path="admin/settings"
                element={
                  <RequireAdmin>
                    <SettingsPage />
                  </RequireAdmin>
                }
              />
            </Route>
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </FedRoleProvider>
      </AuthProvider>
    </ToastProvider>
  );
}
