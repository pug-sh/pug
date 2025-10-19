import { Route } from 'wouter'
import SigninForm from '@/pages/auth/signin'
import SignupForm from '@/pages/auth/signup'

const Router = () => (
  <>
    <Route path='/signup' component={SignupForm} />
    <Route path='/signin' component={SigninForm} />
  </>
)

export default Router
