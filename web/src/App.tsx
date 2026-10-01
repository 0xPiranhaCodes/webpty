import { BrowserRouter, Navigate, Route, Routes } from 'react-router'

import { AccessPage } from './admin/AccessPage'
import { AdminShell } from './admin/AdminShell'
import { AuditPage } from './admin/AuditPage'
import { OverviewPage } from './admin/OverviewPage'
import { SessionsPage } from './admin/SessionsPage'
import { SessionWorkspacePage } from './admin/SessionWorkspacePage'
import { SettingsPage } from './admin/SettingsPage'
import { AdminAuthProvider } from './auth/AdminAuth'
import { RequireAdmin } from './auth/RequireAdmin'
import { JoinPage } from './join/JoinPage'
import { NotFoundPage } from './NotFoundPage'
import { RecordingPlayerPage } from './recordings/RecordingPlayerPage'
import { RecordingsPage } from './recordings/RecordingsPage'

function AdminRoot() {
  return (
    <AdminAuthProvider>
      <RequireAdmin />
    </AdminAuthProvider>
  )
}

export function AppRoutes() {
  return (
    <Routes>
      <Route path="/" element={<Navigate to="/admin" replace />} />
      <Route path="/join" element={<JoinPage />} />
      <Route path="/admin" element={<AdminRoot />}>
        <Route path="sessions/:id" element={<SessionWorkspacePage />} />
        <Route path="recordings/:id" element={<RecordingPlayerPage />} />
        <Route element={<AdminShell />}>
          <Route index element={<OverviewPage />} />
          <Route path="sessions" element={<SessionsPage />} />
          <Route path="recordings" element={<RecordingsPage />} />
          <Route path="access" element={<AccessPage />} />
          <Route path="audit" element={<AuditPage />} />
          <Route path="settings" element={<SettingsPage />} />
          <Route path="*" element={<NotFoundPage />} />
        </Route>
      </Route>
      <Route path="*" element={<NotFoundPage />} />
    </Routes>
  )
}

export default function App() {
  return (
    <BrowserRouter>
      <AppRoutes />
    </BrowserRouter>
  )
}
