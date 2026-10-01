import { Link } from 'react-router'

import { AuthFrame } from './auth/AuthFrame'

export function NotFoundPage() {
  return (
    <AuthFrame>
      <h1>Page not found</h1>
      <p className="muted lede">There is nothing at this address. Shared terminal links start with /join.</p>
      <p>
        <Link to="/admin">Go to the admin overview</Link>
      </p>
    </AuthFrame>
  )
}
