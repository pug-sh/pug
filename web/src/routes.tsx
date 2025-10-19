import { Route } from 'wouter'
import SignupForm from '@/pages/auth/signup'
import SigninForm from '@/pages/auth/signin'

const Router = () => (
  <>
    <Route path='/signup' component={SignupForm} />
    <Route path='/signin' component={SigninForm} />
  </>
)

export default Router
