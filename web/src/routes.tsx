import { Route } from 'wouter'
import AuthProtected from './components/hocs/auth-protected'
import SigninForm from '@/pages/auth/signin'
import SignupForm from '@/pages/auth/signup'
import Dashboard from '@/pages/dashboard'
import Projects from '@/pages/projects'
import Settings from '@/pages/settings'
import Account from '@/pages/account'

const Router = () => (
  <>
    <Route path='/signup' component={SignupForm} />
    <Route path='/signin' component={SigninForm} />
    <AuthProtected>
      <Route path='/' component={Dashboard} />
      <Route path='/projects' component={Projects} />
      <Route path='/settings' component={Settings} />
      <Route path='/account' component={Account} />
    </AuthProtected>
  </>
)

export default Router
