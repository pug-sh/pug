import { atom } from "jotai";
import { jwtDecode } from "jwt-decode";
import { atomWithStorage, RESET } from "jotai/utils";
import { stringStorage } from "./utils";

export const authAtom = atomWithStorage("token", "", stringStorage, {
  getOnInit: true,
});

export const isLoggedInAtom = atom((get) => !!get(authAtom));

export const logoutAtom = atom(null, (_, set) => {
  set(authAtom, RESET);
});

export const loginAtom = atom(null, (_, set, token: string) => {
  set(authAtom, token);
});

interface JwtPayload {
  id: string;
  email: string;
}

export const userInfoAtom = atom((get) => {
  const token = get(authAtom);
  try {
    if (token) {
      return jwtDecode(token) as JwtPayload;
    }
  } catch (error) {
    console.error(error);
  }
});
