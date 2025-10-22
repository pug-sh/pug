import { useAtom } from 'jotai'
import React, { type ReactNode } from 'react'
import { Redirect } from 'wouter'
import { isLoggedInAtom } from '@/atoms/auth'

const AuthProtected: React.FC<{ children: ReactNode }> = ({ children }) => {
  const [isLoggedIn] = useAtom(isLoggedInAtom)

  if (!isLoggedIn) {
    return <Redirect to='/signin' />
  }

  return <>{children}</>
}

export default AuthProtected
