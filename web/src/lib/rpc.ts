import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { AuthService } from '@buf/pushpa_cotton.bufbuild_es/auth/v1/auth_pb';

const apiBaseUrl = "http://localhost:8000";

// const authInterceptor: Interceptor = (next) => async (req) => {
//   const token = localStorage.getItem("token");
//   if (token) {
//     req.header.set("Authorization", `Bearer ${token}`);
//   }

//   return await next(req);
// };

// const transport = createConnectTransport({
//   baseUrl: apiBaseUrl,
//   interceptors: [authInterceptor],
// });

const transportWithoutAuth = createConnectTransport({
  useBinaryFormat: true,
  baseUrl: apiBaseUrl,
});


export const authService = createClient(AuthService, transportWithoutAuth);
