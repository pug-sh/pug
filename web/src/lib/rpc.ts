import { AuthService } from '@buf/pushpa_cotton.bufbuild_es/auth/v1/auth_pb'
import { JourneysService } from '@buf/pushpa_cotton.bufbuild_es/journeys/v1/journeys_pb'
import { ProjectsService } from '@buf/pushpa_cotton.bufbuild_es/projects/v1/projects_pb'
import { createClient, type Interceptor } from '@connectrpc/connect'
import { createConnectTransport } from '@connectrpc/connect-web'

const apiBaseUrl = 'http://localhost:8081'

const authInterceptor: Interceptor = (next) => {
  return async (req) => {
    const token = localStorage.getItem('token')
    if (token) {
      req.header.set('Authorization', `Bearer ${token}`)
    }

    return await next(req)
  }
}

const transport = createConnectTransport({
  useBinaryFormat: true,
  baseUrl: apiBaseUrl,
  interceptors: [authInterceptor],
})

const transportWithoutAuth = createConnectTransport({
  useBinaryFormat: true,
  baseUrl: apiBaseUrl,
})

export const authService = createClient(AuthService, transportWithoutAuth)
export const projectsService = createClient(ProjectsService, transport)
export const journeysService = createClient(JourneysService, transport)
