import { Route } from 'wouter'
import AuthProtected from './components/hocs/auth-protected'
import SigninForm from '@/pages/auth/signin'
import SignupForm from '@/pages/auth/signup'
import Dashboard from '@/pages/dashboard'

const Router = () => (
  <>
    <Route path='/signup' component={SignupForm} />
    <Route path='/signin' component={SigninForm} />
    <AuthProtected><Route path='/' component={Dashboard} /></AuthProtected>
  </>
)

export default Router
