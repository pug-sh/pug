import { createClient } from '@connectrpc/connect'
import { createConnectTransport } from '@connectrpc/connect-web'
import { AuthService } from '../../../clients/typescript/auth/v1/auth_pb.ts'

const apiBaseUrl = 'http://localhost:8000'

const transport = createConnectTransport({
  baseUrl: apiBaseUrl,
  // useBinaryFormat: true,
  fetch: (input: RequestInfo | URL, init: RequestInit = {}) => {
    /*
      TODO: Use interceptor?
      Currently this tries to send an empty bearer token to
      login rpc too! there should be an option choose if we
      want to send the token or not;
      this would be crucial if a case arises where we want to
      call third party serives from the web app
    */
    const token = localStorage.getItem('token')
    const headers = new Headers(init.headers || {})

    if (token) {
      headers.set('Authorization', `Bearer ${token}`)
    }

    return fetch(input, { ...init, headers })
  },
})

export const authService = createClient(AuthService, transport)

export { transport, apiBaseUrl }
