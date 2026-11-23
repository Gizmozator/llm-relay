import { Navigate, Route, Routes } from 'react-router-dom';
import { AuthProvider } from './auth';
import { ToastProvider } from './components/Toast';
import Layout, { RequireAdmin, RequireAuth } from './Layout';
import LoginPage from './pages/Login';
import DashboardPage from './pages/Dashboard';
import UsagePage from './pages/Usage';
import KeysPage from './pages/Keys';
import AgentGuidePage from './pages/AgentGuide';
import SetupGuidePage from './pages/SetupGuide';
import BackendsPage from './pages/Backends';
import ModelsPage from './pages/Models';
import AdminModelsPage from './pages/AdminModels';
import AdminFederationPage from './pages/AdminFederation';
import ServersPage from './pages/Servers';
import UsersPage from './pages/Users';
import SettingsPage from './pages/Settings';

export default function App() {
  return (
    <ToastProvider>
      <AuthProvider>
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
            <Route
              path="admin/servers"
              element={
                <RequireAdmin>
                  <ServersPage />
                </RequireAdmin>
              }
            />
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
      </AuthProvider>
    </ToastProvider>
  );
}
