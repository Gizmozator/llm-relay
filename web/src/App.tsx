import { Navigate, Route, Routes } from 'react-router-dom';
import { AuthProvider } from './auth';
import { ToastProvider } from './components/Toast';
import Layout, { RequireAdmin, RequireAuth } from './Layout';
import LoginPage from './pages/Login';
import UsagePage from './pages/Usage';
import KeysPage from './pages/Keys';
import BackendsPage from './pages/Backends';
import ModelsPage from './pages/Models';
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
            <Route index element={<UsagePage admin={false} />} />
            <Route path="keys" element={<KeysPage />} />
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
                  <ModelsPage />
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
