import { Route } from 'wouter'
import AuthProtected from './components/hocs/auth-protected'
import Account from '@/pages/account'
import SigninForm from '@/pages/auth/signin'
import SignupForm from '@/pages/auth/signup'
import Campaigns from '@/pages/campaigns'
import NewCampaign from '@/pages/campaigns/new'
import Dashboard from '@/pages/dashboard'
import Journeys from '@/pages/journeys'
import Projects from '@/pages/projects'
import NewProject from '@/pages/projects/new'
import Settings from '@/pages/settings'

const Router = () => (
  <>
    <Route path='/signup' component={SignupForm} />
    <Route path='/signin' component={SigninForm} />
    <AuthProtected>
      <Route path='/' component={Dashboard} />
      <Route path='/projects' component={Projects} />
      <Route path='/projects/new' component={NewProject} />
      <Route path='/settings' component={Settings} />
      <Route path='/account' component={Account} />
      <Route path='/journeys' component={Journeys} />
      <Route path='/campaigns' component={Campaigns} />
      <Route path='/campaigns/new' component={NewCampaign} />
    </AuthProtected>
  </>
)

export default Router
